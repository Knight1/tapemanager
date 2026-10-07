package main

import (
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
	"github.com/Knight1/tapemanager/internal/ibmfw"
)

// firmwareKeys returns the keys IBM firmware signatures are checked with;
// tests replace it with test keys.
var firmwareKeys = ibmfw.IBMKeys

// checkIBMFirmware runs the drive's own image checks: container, section
// and image checksums, and both IBM signatures. It returns the report, or
// an error listing every failed check.
func checkIBMFirmware(image []byte) (*ibmfw.Report, error) {
	keys, err := firmwareKeys()
	if err != nil {
		return nil, fmt.Errorf("loading the IBM signing keys: %w", err)
	}
	r, err := ibmfw.Inspect(image, keys, time.Now())
	if err != nil {
		return nil, err
	}
	if !r.Valid() {
		return r, fmt.Errorf("the image failed its checks, so it is damaged or was changed; nothing was sent:\n  - %s\n'tapemgr drive inspect-firmware --file IMAGE' shows the details",
			strings.Join(r.Problems, "\n  - "))
	}
	return r, nil
}

// checkPlatform compares the interface and form factor the image is for
// with what the drive reports, as the drive itself does.
func checkPlatform(d drive.Device, q drive.Inquiry, r *ibmfw.Report) (string, error) {
	fb, err := drive.ReadFirmwareBuild(d, q)
	if err != nil || fb.Platform == "" {
		return fmt.Sprintf("for %s drives (this drive does not report its type; the drive checks it)", r.Platform), nil
	}
	if !ibmfw.PlatformMatches(r.Platform, fb.Platform) {
		return "", fmt.Errorf("the image is for %s drives, but this drive is %s", r.Platform, fb.Platform)
	}
	return fmt.Sprintf("for %s drives, matches this drive", r.Platform), nil
}

func cmdDriveInspectFirmware(args []string, stdout, stderr io.Writer) int {
	fs, _ := newFlagSet("drive inspect-firmware", stderr)
	file := fs.String("file", "", "firmware image to inspect")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *file == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: tapemgr drive inspect-firmware --file IMAGE")
		return exitUsage
	}
	image, err := readFirmwareFile(*file)
	if err != nil {
		return fail(stderr, err)
	}
	keys, err := firmwareKeys()
	if err != nil {
		return fail(stderr, fmt.Errorf("loading the IBM signing keys: %w", err))
	}
	now := time.Now()
	r, err := ibmfw.Inspect(image, keys, now)
	if err != nil {
		return fail(stderr, fmt.Errorf("%s: %w", *file, err))
	}
	printFirmwareReport(stdout, *file, image, r, keys, now)
	if !r.Valid() {
		return exitFailure
	}
	return exitOK
}

func printFirmwareReport(w io.Writer, file string, image []byte, r *ibmfw.Report, keys ibmfw.Keys, now time.Time) {
	sum := sha256.Sum256(image)
	h := r.Header
	model := "-"
	if img, err := drive.ParseIBMImage(image); err == nil {
		model = img.ModelID
	}
	lenNote := "matches the file"
	if int64(h.Length) != int64(len(image)) {
		lenNote = "does NOT match the file"
	}
	fmt.Fprintf(w, `Image:     %s
           %s, SHA-256 %s
Header:    level %s, length %d (%s), format %#08x
           load ID %x, model %s, hardware IDs %#08x to %#08x
`, file, archive.FormatBytes(int64(len(image))), hex.EncodeToString(sum[:]),
		orDash(h.Level), h.Length, lenNote, h.Version, h.LoadID, model, h.HWMin, h.HWMax)

	fmt.Fprintln(w, "\nAttributes:")
	for _, a := range r.Attrs {
		fmt.Fprintf(w, "  %3d  %s\n", a.ID, a.Value)
	}

	fmt.Fprintln(w, "\nSections:")
	fmt.Fprintf(w, "  %-4s  %-8s  %-10s  %-10s  %-10s  %-8s  %s\n", "name", "flags", "offset", "size", "load at", "sum", "CRC")
	for _, s := range r.Sections {
		sumS, crcS := s.Sum.String(), s.CRC.String()
		if s.Required == 0 {
			sumS, crcS = "no data", ""
		}
		row := fmt.Sprintf("  %-4s  %08x  0x%08x  0x%08x  0x%08x  %-8s  %s", s.Name, s.Flags, s.Offset, s.Size, s.LoadAddr, sumS, crcS)
		fmt.Fprintln(w, strings.TrimRight(row, " "))
	}

	fmt.Fprintln(w, "\nRecords, from the end of the image:")
	for _, rec := range r.Records {
		fmt.Fprintf(w, "  0x%08x  %-18s  %s\n", rec.Desc, rec.Name(), describeRecord(rec))
	}

	fmt.Fprintln(w)
	if r.Signed > 0 {
		fmt.Fprintf(w, "Signed:    bytes 0x0 to %#x; the last %d bytes are not covered by any signature\n", r.Signed-1, len(image)-r.Signed)
	} else {
		fmt.Fprintln(w, "Signed:    no part of the image is covered by a valid signature")
	}
	for _, tag := range []byte{ibmfw.TagSigSHA256, ibmfw.TagSigSHA1} {
		if k, ok := keys[tag]; ok {
			fmt.Fprintf(w, "Key:       %s, certificate SHA-256 %s\n", k.Name, orDash(k.Fingerprint))
		}
	}
	if r.Valid() {
		fmt.Fprintln(w, "Result:    all checks passed")
		return
	}
	fmt.Fprintln(w, "Result:    FAILED")
	for _, p := range r.Problems {
		fmt.Fprintf(w, "  - %s\n", p)
	}
}

func describeRecord(rec ibmfw.Record) string {
	switch rec.Tag {
	case ibmfw.TagSigSHA1, ibmfw.TagSigSHA256:
		if rec.Status != ibmfw.OK {
			return fmt.Sprintf("covers 0x0 to %#x, INVALID: %s", rec.Covered[1]-1, rec.Detail)
		}
		s := fmt.Sprintf("covers %#x to %#x, VALID with %s", rec.Covered[0], rec.Covered[1]-1, rec.KeyName)
		if rec.Fallback {
			s += " (over the drive's fallback range, without the first 32 header bytes)"
		}
		if rec.KeyExpired {
			s += "; its certificate has expired, which the drive ignores"
		}
		return s
	case ibmfw.TagSum, ibmfw.TagCRC:
		if rec.Status != ibmfw.OK {
			return fmt.Sprintf("covers 0x0 to %#x, %s", rec.Covered[1]-1, orDash(rec.Detail))
		}
		return fmt.Sprintf("covers 0x0 to %#x, OK", rec.Covered[1]-1)
	}
	if rec.Status != ibmfw.OK {
		return orDash(rec.Detail)
	}
	if len(rec.Denied) == 0 {
		return "not signed; excludes no drive type"
	}
	idx := make([]string, len(rec.Denied))
	for i, d := range rec.Denied {
		idx[i] = fmt.Sprint(d)
	}
	return "not signed; excludes drive type index " + strings.Join(idx, ", ")
}

// readFirmwareFile reads a regular file of plausible firmware size.
func readFirmwareFile(name string) ([]byte, error) {
	st, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	if st.Size() < minFirmwareSize || st.Size() > maxFirmwareSize {
		return nil, fmt.Errorf("%s has %d bytes, which is not a plausible firmware image", name, st.Size())
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	// The file may have grown since the size check.
	if len(b) > maxFirmwareSize {
		return nil, errors.New(name + " grew while it was read")
	}
	return b, nil
}
