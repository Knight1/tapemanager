package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Knight1/tapemanager/internal/archive"
	"github.com/Knight1/tapemanager/internal/drive"
)

// How long to wait for the drive after a firmware update; tests shorten it.
var (
	firmwareWait     = 15 * time.Minute
	firmwareInterval = 5 * time.Second
)

// Firmware images below this size are certainly not drive firmware.
const (
	minFirmwareSize = 64 << 10
	maxFirmwareSize = 64 << 20
)

func cmdDriveFirmware(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, df := newDriveFlagSet("drive firmware", stderr)
	file := fs.String("file", "", "firmware image from the drive vendor")
	yes := fs.Bool("yes", false, "start without asking for the drive's serial number")
	dry := fs.Bool("dry-run", false, "check the drive and the file, but do not update")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *file == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: tapemgr drive firmware --file IMAGE [--dry-run] [--yes] [--device /dev/sgN]")
		return exitUsage
	}

	st, err := os.Stat(*file)
	if err != nil {
		return fail(stderr, err)
	}
	if !st.Mode().IsRegular() {
		return fail(stderr, fmt.Errorf("%s is not a regular file", *file))
	}
	if st.Size() < minFirmwareSize || st.Size() > maxFirmwareSize {
		return fail(stderr, fmt.Errorf("%s has %d bytes, which is not a plausible firmware image", *file, st.Size()))
	}
	image, err := os.ReadFile(*file)
	if err != nil {
		return fail(stderr, err)
	}
	sum := sha256.Sum256(image)

	d, f, err := df.open()
	if err != nil {
		return fail(stderr, err)
	}
	defer func() {
		if d != nil {
			d.Close()
		}
	}()
	if err := requireTape(d); err != nil {
		return fail(stderr, err)
	}
	q, err := drive.ReadInquiry(d)
	if err != nil {
		return fail(stderr, err)
	}
	serial, err := drive.ReadSerial(d)
	if err != nil || serial == "" {
		return fail(stderr, fmt.Errorf("cannot read the drive's serial number: %v", err))
	}

	// The drive must be idle: no cartridge, no LTFS mount.
	mps, err := mountPoints(f, serial)
	if err == nil && len(f.Names) == 0 {
		mps, err = ltfsMounts()
	}
	if err != nil {
		return fail(stderr, fmt.Errorf("checking mounts: %w", err))
	}
	if len(mps) > 0 {
		return fail(stderr, fmt.Errorf("a tape is mounted at %s; unmount it and eject the cartridge first", strings.Join(mps, ", ")))
	}
	if err := drive.TestUnitReady(d); !errors.Is(err, drive.ErrNoMedium) {
		return fail(stderr, errors.New("a cartridge is loaded; eject it first ('tapemgr drive eject')"))
	}
	buf, err := drive.ReadMicrocodeBuffer(d)
	if err != nil {
		return fail(stderr, fmt.Errorf("the drive does not report a microcode buffer: %w", err))
	}
	chunk, err := drive.CheckFirmwareImage(buf, image, drive.DefaultFirmwareChunk)
	if err != nil {
		return fail(stderr, err)
	}

	fmt.Fprintf(stdout, `Drive:     %s %s, serial %s (%s)
Firmware:  %s now
Image:     %s
           %s, SHA-256 %s
Buffer:    %s, sent in pieces of %s

Compare the SHA-256 with the one the vendor publishes for this file. Make
sure the image is meant for this exact drive model.
`, q.Vendor, q.Product, serial, f.SG, q.Revision, *file, archive.FormatBytes(int64(len(image))),
		hex.EncodeToString(sum[:]), archive.FormatBytes(int64(buf.Capacity)), archive.FormatBytes(int64(chunk)))
	if *dry {
		fmt.Fprintln(stdout, "\nDry run: the drive and the file passed all checks. Nothing was sent.")
		return exitOK
	}
	fmt.Fprint(stdout, `
The drive saves the new firmware and restarts, which takes several minutes.
Do not power off the drive or this computer until this command has finished:
an interrupted save can leave the drive unusable.
`)
	if !*yes {
		fmt.Fprintf(stdout, "\nType the drive's serial number (%s) to start: ", serial)
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(answer) != serial {
			fmt.Fprintln(stdout, "Aborted. Nothing was sent to the drive.")
			return exitFailure
		}
	}

	prog := progressOut(stderr)
	err = drive.UpdateFirmware(d, image, chunk, func(done, total int) {
		if prog != nil {
			fmt.Fprintf(prog, "\rSending firmware: %3d%%", done*100/total)
		}
	})
	if prog != nil {
		fmt.Fprintln(prog)
	}
	d.Close()
	d = nil
	var fe *drive.FirmwareError
	switch {
	case errors.As(err, &fe) && fe.Offset+chunk < len(image):
		fmt.Fprintf(stderr, "tapemgr: %v\nThe image was not complete, so the drive keeps its current firmware. Check the cable and the file, then try again.\n", err)
		return exitFailure
	case fe != nil:
		// The last piece failed: the drive may still have saved it.
		fmt.Fprintf(stderr, "tapemgr: %v\nWaiting for the drive to find out whether it took the update.\n", err)
	case err != nil:
		return fail(stderr, err)
	}

	fmt.Fprintln(stdout, "Firmware sent. Waiting for the drive to restart...")
	after, werr := drive.WaitForDrive(func() (drive.Device, error) { return openDevice(f.SG) }, firmwareWait, firmwareInterval)
	if werr != nil {
		fmt.Fprintf(stderr, "tapemgr: %v\nDo not power it off yet; check it again in a few minutes with 'tapemgr drive info'.\n", werr)
		return exitFailure
	}
	if after.Revision == q.Revision {
		fmt.Fprintf(stdout, "The drive still reports firmware %s. It rejected the image, or the image has the same level.\n", after.Revision)
		return exitFailure
	}
	fmt.Fprintf(stdout, "Firmware updated: %s -> %s.\n", q.Revision, after.Revision)
	return exitOK
}
