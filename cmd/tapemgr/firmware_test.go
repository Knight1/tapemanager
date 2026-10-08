package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/drive/drivetest"
	"github.com/Knight1/tapemanager/internal/ibmfw"
)

// firmwareFile writes an image with an IBM header matching the fake drive.
func firmwareFile(t *testing.T, n int) (string, []byte) {
	t.Helper()
	return writeImage(t, drivetest.IBMImage(drivetest.FakeLoadID, drivetest.FakeModelID, "E6R4", n, func(b []byte) { rand.Read(b) }))
}

func writeImage(t *testing.T, b []byte) (string, []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "image.fmrz")
	os.WriteFile(p, b, 0o644)
	return p, b
}

func firmwareFake(t *testing.T, img []byte) *drivetest.Fake {
	t.Helper()
	f := drivetest.New()
	f.NoMedium = true
	f.ImageSize, f.NewRevision = len(img), "E6R4"
	useFake(t, f, fakeFound, nil)
	oldWait, oldInterval := firmwareWait, firmwareInterval
	firmwareWait, firmwareInterval = time.Second, time.Millisecond
	t.Cleanup(func() { firmwareWait, firmwareInterval = oldWait, oldInterval })
	return f
}

func TestCLIFirmwareDryRunAndConfirm(t *testing.T) {
	file, img := firmwareFile(t, 600<<10)
	f := firmwareFake(t, img)

	code, out := runWithInput("", "drive", "firmware", "--file", file, "--dry-run")
	if code != 0 || !strings.Contains(out, "Firmware:  E6R3 now") || !strings.Contains(out, "SHA-256") || !strings.Contains(out, "Nothing was sent") || f.Downloads != 0 ||
		!strings.Contains(out, "image for TESTID01 (load ID 11223344), matches this drive; image level E6R4, built 2026/01/02") {
		t.Fatalf("dry run: %d %s", code, out)
	}
	code, out = runWithInput("wrong\n", "drive", "firmware", "--file", file)
	if code != exitFailure || !strings.Contains(out, "Aborted") || f.Downloads != 0 {
		t.Fatalf("wrong serial: %d %s", code, out)
	}
	code, out = runWithInput("0000000001\n", "drive", "firmware", "--file", file)
	if code != 0 || !strings.Contains(out, "Firmware updated: E6R3 -> E6R4") || !bytes.Equal(f.Received, img) {
		t.Fatalf("update: %d %s", code, out)
	}
	if f.Downloads != 3 { // 600 KiB in 256 KiB pieces
		t.Fatalf("%d pieces", f.Downloads)
	}
}

func TestCLIFirmwareRefuses(t *testing.T) {
	file, img := firmwareFile(t, 300<<10)
	f := firmwareFake(t, img)
	f.NoMedium = false
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--yes"); code != exitFailure || !strings.Contains(out, "cartridge is loaded") {
		t.Fatalf("cartridge: %s", out)
	}
	// Ejected but not taken out: the drive reports "load needed".
	f.InSlot = true
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--yes"); code != exitFailure || !strings.Contains(out, "still in the drive's slot; take it out") {
		t.Fatalf("in slot: %s", out)
	}
	f.InSlot = false
	f.Fail = map[byte]*drive.CommandError{0x00: {Op: 0x00, Status: 0x02, Key: drive.SenseNotReady, ASC: 0x04, ASCQ: 0x01}}
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--yes"); code != exitFailure || !strings.Contains(out, "not idle") {
		t.Fatalf("becoming ready: %s", out)
	}
	f.Fail = nil
	if f.Downloads != 0 {
		t.Fatal("sent firmware to a busy drive")
	}
	f.NoMedium = true
	mountPoints = func(_ drive.Found, _ string) ([]string, error) { return []string{"/mnt/ltfs"}, nil }
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--yes"); code != exitFailure || !strings.Contains(out, "mounted at /mnt/ltfs") {
		t.Fatalf("mounted: %s", out)
	}
	mountPoints = func(drive.Found, string) ([]string, error) { return nil, nil }
	other, _ := writeImage(t, drivetest.IBMImage([]byte{9, 9, 9, 9}, drivetest.FakeModelID, "E6R4", 300<<10, nil))
	if code, out := runWithInput("", "drive", "firmware", "--file", other, "--yes"); code != exitFailure || !strings.Contains(out, "another drive model") {
		t.Fatalf("other model: %s", out)
	}
	plain, _ := writeImage(t, make([]byte, 300<<10))
	if code, out := runWithInput("", "drive", "firmware", "--file", plain, "--yes"); code != exitFailure || !strings.Contains(out, "not an IBM tape drive firmware image") {
		t.Fatalf("plain file: %s", out)
	}
	big, _ := writeImage(t, make([]byte, 16<<20))
	if code, out := runWithInput("", "drive", "firmware", "--file", big, "--yes"); code != exitFailure || !strings.Contains(out, "not a plausible firmware image") {
		t.Fatalf("too large: %s", out)
	}
	small, _ := writeImage(t, make([]byte, 1000))
	if code, out := runWithInput("", "drive", "firmware", "--file", small, "--yes"); code != exitFailure || !strings.Contains(out, "not a plausible firmware image") {
		t.Fatalf("small: %s", out)
	}
	if code, _ := runWithInput("", "drive", "firmware", "--file", filepath.Join(t.TempDir(), "missing"), "--yes"); code != exitFailure {
		t.Fatal("missing file")
	}
	if code, _ := runWithInput("", "drive", "firmware"); code != exitUsage {
		t.Fatal("missing --file")
	}
	f.DeviceType = 0
	f.BufferCapacity = 0
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--yes"); code != exitFailure || !strings.Contains(out, "not a tape drive") {
		t.Fatalf("disk: %s", out)
	}
	if f.Downloads != 0 {
		t.Fatal("firmware sent despite a refusal")
	}
}

func TestCLIFirmwareRejectedAndInterrupted(t *testing.T) {
	file, img := firmwareFile(t, 600<<10)
	f := firmwareFake(t, img)
	f.NewRevision = ""
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--yes"); code != exitFailure || !strings.Contains(out, "still reports firmware E6R3") {
		t.Fatalf("rejected: %s", out)
	}

	f = firmwareFake(t, img)
	f.FailAt = 256 << 10
	code, out := runWithInput("", "drive", "firmware", "--file", file, "--yes")
	if code != exitFailure || !strings.Contains(out, "keeps its current firmware") {
		t.Fatalf("interrupted: %s", out)
	}
	if q := f.Revision; q != "" && q != "E6R3" {
		t.Fatalf("revision %s", q)
	}

	// The last piece fails, but the drive saved the image anyway.
	f = firmwareFake(t, img)
	f.FailAt = 512 << 10
	f.ImageSize = 512 << 10
	code, out = runWithInput("", "drive", "firmware", "--file", file, "--yes")
	if !strings.Contains(out, "Waiting for the drive to find out") {
		t.Fatalf("last piece: %d %s", code, out)
	}
}

// An image larger than the 5 MiB the drive reports for buffer 0 goes
// through, as real IBM LTO-6 images do.
func TestCLIFirmwareLargerThanBuffer(t *testing.T) {
	file, img := firmwareFile(t, 6<<20)
	f := firmwareFake(t, img)
	f.BufferCapacity = 5 << 20
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--yes"); code != 0 || !strings.Contains(out, "E6R3 -> E6R4") {
		t.Fatalf("%d %s", code, out)
	}
}

// Drives of other vendors get no model check; the drive decides.
func TestCLIFirmwareOtherVendor(t *testing.T) {
	file, img := writeImage(t, make([]byte, 300<<10))
	f := firmwareFake(t, img)
	f.Vendor = "HP"
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--dry-run"); code != 0 || !strings.Contains(out, "no check for this vendor") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCLIFirmwareSameLevel(t *testing.T) {
	file, img := writeImage(t, drivetest.IBMImage(drivetest.FakeLoadID, drivetest.FakeModelID, "E6R3", 300<<10, nil))
	firmwareFake(t, img)
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--dry-run"); code != 0 || !strings.Contains(out, "the level already installed") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestFirmwareProgress(t *testing.T) {
	const chunk = 256
	for _, c := range []struct {
		done, total int
		saving      bool
	}{
		{256, 1024, false},
		{512, 1024, false},
		{768, 1024, true}, // the next piece is the last
		{1000, 1024, true},
		{1024, 1024, false},
		{256, 257, true},
	} {
		got := firmwareProgress(c.done, c.total, chunk)
		// Same width for every line, no escape sequences, fits 80 columns.
		if !strings.HasPrefix(got, "\rSending firmware: ") || strings.Contains(got, "restarts") != c.saving ||
			len(got) != 1+firmwareProgressWidth || firmwareProgressWidth >= 80 || strings.ContainsRune(got, 0x1b) {
			t.Errorf("%d/%d: %q", c.done, c.total, got)
		}
	}
	if got := firmwareProgress(1024, 1024, chunk); strings.TrimRight(got, " ") != "\rSending firmware: 100%" {
		t.Errorf("done: %q", got)
	}
	// Through the terminal filter, the line arrives unchanged.
	var b strings.Builder
	termSafe{&b}.Write([]byte(firmwareProgress(768, 1024, chunk)))
	if b.String() != firmwareProgress(768, 1024, chunk) {
		t.Errorf("filtered: %q", b.String())
	}
}

// IBM LTO-6 firmware H991 reports an offset boundary byte (0x86) that the
// standard does not define; E6R3 reports a valid one. Both must work.
func TestCLIFirmwareUndefinedBoundary(t *testing.T) {
	for _, b := range []byte{0x86, 0xFF, 0} {
		file, img := firmwareFile(t, 600<<10)
		f := firmwareFake(t, img)
		f.Boundary = b
		code, out := runWithInput("0000000001\n", "drive", "firmware", "--file", file)
		if code != 0 || !strings.Contains(out, "Firmware updated") || !bytes.Equal(f.Received, img) || f.Downloads != 3 {
			t.Fatalf("boundary %#x: %d %d pieces %s", b, code, f.Downloads, out)
		}
		if note := strings.Contains(out, "offset alignment 0x86, which the SCSI standard does not define"); note != (b == 0x86) {
			t.Fatalf("boundary %#x: %s", b, out)
		}
	}
}

// The drive's own image checks run before anything is sent.
func TestCLIFirmwareImageChecks(t *testing.T) {
	file, img := firmwareFile(t, 600<<10)
	f := firmwareFake(t, img)
	code, out := runWithInput("", "drive", "firmware", "--file", file, "--dry-run")
	if code != 0 || !strings.Contains(out, "Checks:    container, section and image checksums, and both IBM signatures passed; for sas_hh drives, matches this drive") ||
		!strings.Contains(out, "for sas_hh drives, matches this drive") {
		t.Fatalf("valid: %d %s", code, out)
	}

	// A changed byte inside the image: refused, nothing sent.
	bad := bytes.Clone(img)
	bad[len(bad)/2] ^= 1
	badFile, _ := writeImage(t, bad)
	code, out = runWithInput("0000000001\n", "drive", "firmware", "--file", badFile)
	if code != exitFailure || !strings.Contains(out, "failed its checks") || !strings.Contains(out, "section vmli: word sum mismatch") || f.Downloads != 0 {
		t.Fatalf("changed: %d %d %s", code, f.Downloads, out)
	}

	// Signed with other keys: refused.
	old := firmwareKeys
	t.Cleanup(func() { firmwareKeys = old })
	firmwareKeys = ibmfw.IBMKeys
	code, out = runWithInput("0000000001\n", "drive", "firmware", "--file", file)
	if code != exitFailure || !strings.Contains(out, "SHA-256 signature: does not verify with TAPEFIRMWARE") || f.Downloads != 0 {
		t.Fatalf("other keys: %d %s", code, out)
	}
	firmwareKeys = old

	// An image for another interface: refused.
	f.Platform = "fc_hh"
	code, out = runWithInput("0000000001\n", "drive", "firmware", "--file", file)
	if code != exitFailure || !strings.Contains(out, "the image is for sas_hh drives, but this drive is fc_hh") || f.Downloads != 0 {
		t.Fatalf("platform: %d %s", code, out)
	}
}

func TestCLIInspectFirmware(t *testing.T) {
	file, img := firmwareFile(t, 300<<10)
	code, out, errOut := runCmd(t, "drive", "inspect-firmware", "--file", file)
	for _, want := range []string{
		"level E6R4", "(matches the file)", "model TESTID01", "hardware IDs 0x00010060 to 0x00010061",
		"    9  sas_hh", "vmli", "SHA-256 signature   covers 0x0 to", "VALID with TEST_SHA256",
		"image CRC", "excludes no drive type", "the last 332 bytes are not covered", "Result:    all checks passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if code != 0 || t.Failed() {
		t.Fatalf("%d %s %s", code, out, errOut)
	}

	bad := bytes.Clone(img)
	bad[0x100] ^= 1
	badFile, _ := writeImage(t, bad)
	code, out, _ = runCmd(t, "drive", "inspect-firmware", "--file", badFile)
	if code != exitFailure || !strings.Contains(out, "Result:    FAILED") || !strings.Contains(out, "INVALID") {
		t.Fatalf("changed: %d %s", code, out)
	}
	plain, _ := writeImage(t, make([]byte, 100<<10))
	if code, _, errOut := runCmd(t, "drive", "inspect-firmware", "--file", plain); code != exitFailure || !strings.Contains(errOut, "not an IBM tape drive firmware image") {
		t.Fatalf("plain: %d %s", code, errOut)
	}
	if code, _, _ := runCmd(t, "drive", "inspect-firmware"); code != exitUsage {
		t.Fatal("missing --file")
	}
}

// The image is checked before the drive is touched: a drive is found but
// cannot be opened (TestMain makes every open fail), so a changed image is
// refused before the open while an intact one reaches it. resolveDrive is
// stubbed so the test does not depend on the host actually having a drive.
func TestCLIFirmwareChecksImageFirst(t *testing.T) {
	oldResolve := resolveDrive
	resolveDrive = func(string, string) (drive.Found, error) { return fakeFound, nil }
	t.Cleanup(func() { resolveDrive = oldResolve })

	file, img := firmwareFile(t, 300<<10)
	bad := bytes.Clone(img)
	bad[len(bad)/2] ^= 1
	badFile, _ := writeImage(t, bad)
	// The changed image is refused before the drive is opened.
	code, out := runWithInput("", "drive", "firmware", "--file", badFile, "--yes")
	if code != exitFailure || !strings.Contains(out, "failed its checks") || strings.Contains(out, "no drive in tests") {
		t.Fatalf("changed image: %d %s", code, out)
	}
	// An intact image gets past the check and on to the drive.
	code, out = runWithInput("", "drive", "firmware", "--file", file, "--yes")
	if code != exitFailure || strings.Contains(out, "failed its checks") || !strings.Contains(out, "no drive in tests") {
		t.Fatalf("intact image: %d %s", code, out)
	}
}

// --skip-image-check sends an image that fails the checks, says so before
// asking, and still refuses an image for another interface.
func TestCLIFirmwareSkipImageCheck(t *testing.T) {
	file, img := firmwareFile(t, 600<<10)
	f := firmwareFake(t, img)
	bad := bytes.Clone(img)
	bad[len(bad)/2] ^= 1
	badFile, _ := writeImage(t, bad)

	code, out := runWithInput("0000000001\n", "drive", "firmware", "--file", badFile, "--skip-image-check")
	if code != 0 || !strings.Contains(out, "Checks:    SKIPPED (--skip-image-check)") || !strings.Contains(out, "for sas_hh drives, matches this drive") ||
		!bytes.Equal(f.Received, bad) || strings.Index(out, "SKIPPED") > strings.Index(out, "Type the drive's serial number") {
		t.Fatalf("skipped: %d %s", code, out)
	}

	f2 := firmwareFake(t, img)
	f2.Platform = "fc_hh"
	code, out = runWithInput("0000000001\n", "drive", "firmware", "--file", file, "--skip-image-check")
	if code != exitFailure || !strings.Contains(out, "this drive is fc_hh") || f2.Downloads != 0 {
		t.Fatalf("platform with skip: %d %s", code, out)
	}
}
