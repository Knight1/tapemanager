package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Knight1/tapemanager/internal/archive"
	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/ibmfw"
)

// How long to wait for the drive after a firmware update; tests shorten it.
var (
	firmwareWait     = 15 * time.Minute
	firmwareInterval = 5 * time.Second
)

// Firmware images below this size are certainly not drive firmware.
const (
	minFirmwareSize = 64 << 10
	maxFirmwareSize = 16<<20 - 1 // the limit of WRITE BUFFER's offset field
)

func cmdDriveFirmware(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, df := newDriveFlagSet("drive firmware", stderr)
	file := fs.String("file", "", "firmware image from the drive vendor")
	yes := fs.Bool("yes", false, "start without asking for the drive's serial number")
	dry := fs.Bool("dry-run", false, "check the drive and the file, but do not update")
	skipCheck := fs.Bool("skip-image-check", false, "do not check an IBM image's checksums and signatures before sending (the drive still checks them)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *file == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: tapemgr drive firmware --file IMAGE [--dry-run] [--yes] [--skip-image-check] [--device /dev/sgN]")
		return exitUsage
	}

	image, err := readFirmwareFile(*file)
	if err != nil {
		return fail(stderr, err)
	}
	sum := sha256.Sum256(image)
	// An IBM image is checked first, the same way 'drive inspect-firmware'
	// does, so a damaged or changed file is refused before the drive is
	// touched. Other files are left to the drive.
	var report *ibmfw.Report
	if !*skipCheck {
		report, err = checkIBMFirmware(image)
		if err != nil && !errors.Is(err, ibmfw.ErrNotImage) {
			return fail(stderr, fmt.Errorf("%s: %w", *file, err))
		}
	}

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
	switch err := drive.TestUnitReady(d); {
	case errors.Is(err, drive.ErrNoMedium):
	case errors.Is(err, drive.ErrNotLoaded):
		// After an eject the cartridge stays in the slot until taken out.
		return fail(stderr, errors.New("the ejected cartridge is still in the drive's slot; take it out, then run this again"))
	case err == nil:
		return fail(stderr, errors.New("a cartridge is loaded; eject it first ('tapemgr drive eject')"))
	default:
		return fail(stderr, fmt.Errorf("the drive is not idle (%v); wait a moment and run this again", err))
	}
	buf, err := drive.ReadMicrocodeBuffer(d)
	if err != nil {
		return fail(stderr, fmt.Errorf("the drive does not report a microcode buffer: %w", err))
	}
	chunk, err := drive.CheckFirmwareImage(buf, image, drive.DefaultFirmwareChunk)
	if err != nil {
		return fail(stderr, err)
	}
	pieces := archive.FormatBytes(int64(chunk))
	if buf.Boundary == drive.BoundaryUndefined {
		pieces += fmt.Sprintf(" (the drive reports offset alignment 0x%02x, which the SCSI standard does not define; pieces this size suit any alignment)", buf.Raw)
	}
	// IBM images name the drive type they are for; check it before sending.
	ibm, err := drive.CheckIBMImage(d, q, image)
	if err != nil {
		return fail(stderr, fmt.Errorf("%s: %w", *file, err))
	}
	model := "no check for this vendor; the drive checks the image itself"
	checks := "none for this vendor; the drive checks the image itself"
	if ibm != nil {
		checks = "container, section and image checksums, and both IBM signatures passed"
		if *skipCheck {
			checks = "SKIPPED (--skip-image-check): checksums and signatures were not checked; the drive still checks them"
			// The interface check needs no intact image, only its
			// attributes, and still protects against the wrong file.
			report, _ = ibmfw.Inspect(image, nil, time.Now())
		}
		if report != nil && report.Platform != "" {
			platform, err := checkPlatform(d, q, report)
			if err != nil {
				return fail(stderr, fmt.Errorf("%s: %w", *file, err))
			}
			checks += "; " + platform
		}
		model = fmt.Sprintf("image for %s (load ID %x), matches this drive; image level %s", ibm.ModelID, ibm.LoadID, orDash(ibm.Level))
		if ibm.Built != "" {
			model += ", built " + ibm.Built
		}
		if ibm.Level == q.Revision {
			model += " (the level already installed)"
		}
	}

	fmt.Fprintf(stdout, `Drive:     %s %s, serial %s (%s)
Firmware:  %s now
Image:     %s
           %s, SHA-256 %s
Model:     %s
Checks:    %s
Sent in:   pieces of %s

Compare the SHA-256 with the one the vendor publishes for this file. Make
sure the image is meant for this exact drive model.
`, q.Vendor, q.Product, serial, f.SG, q.Revision, *file, archive.FormatBytes(int64(len(image))),
		hex.EncodeToString(sum[:]), model, checks, pieces)
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
			fmt.Fprint(prog, firmwareProgress(done, total, chunk))
		}
	})
	if prog != nil {
		fmt.Fprintln(prog)
	}
	d.Close()
	d = nil
	fe, isFE := errors.AsType[*drive.FirmwareError](err)
	switch {
	case isFE && fe.Offset+chunk < len(image):
		fmt.Fprintf(stderr, "tapemgr: %v\nThe image was not complete, so the drive keeps its current firmware. Check the cable and the file, then try again.\n", err)
		return exitFailure
	case isFE:
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

// firmwareProgress returns the progress line shown after done of total
// bytes were sent. The drive answers the last piece only after it saved the
// image to flash, often after its restart, so the line says so before that
// piece is sent instead of seeming to hang. Every line is padded to the
// same width, so a shorter line overwrites a longer one without leftovers;
// escape sequences cannot be used because stderr shows them as text. The
// width stays below 80 columns, since a wrapped line cannot be overwritten.
func firmwareProgress(done, total, chunk int) string {
	line := fmt.Sprintf("Sending firmware: %3d%%", done*100/total)
	if done < total && total-done <= chunk {
		line += firmwareSavingNote
	}
	return fmt.Sprintf("\r%-*s", firmwareProgressWidth, line)
}

const (
	firmwareSavingNote    = ", the drive saves it and restarts (minutes)"
	firmwareProgressWidth = len("Sending firmware: 100%") + len(firmwareSavingNote)
)
