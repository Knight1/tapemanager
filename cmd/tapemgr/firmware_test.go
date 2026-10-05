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
)

func firmwareFile(t *testing.T, n int) (string, []byte) {
	t.Helper()
	b := make([]byte, n)
	rand.Read(b)
	p := filepath.Join(t.TempDir(), "E6R4.ro")
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
	if code != 0 || !strings.Contains(out, "Firmware:  E6R3 now") || !strings.Contains(out, "SHA-256") || !strings.Contains(out, "Nothing was sent") || f.Downloads != 0 {
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
	f.NoMedium = true
	mountPoints = func(_ drive.Found, _ string) ([]string, error) { return []string{"/mnt/ltfs"}, nil }
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--yes"); code != exitFailure || !strings.Contains(out, "mounted at /mnt/ltfs") {
		t.Fatalf("mounted: %s", out)
	}
	mountPoints = func(drive.Found, string) ([]string, error) { return nil, nil }
	f.BufferCapacity = 100 << 10
	if code, out := runWithInput("", "drive", "firmware", "--file", file, "--yes"); code != exitFailure || !strings.Contains(out, "larger than the drive's microcode buffer") {
		t.Fatalf("buffer: %s", out)
	}
	small, _ := firmwareFile(t, 1000)
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
