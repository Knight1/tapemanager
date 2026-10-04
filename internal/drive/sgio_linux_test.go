package drive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"
)

func TestSGIOHeaderLayout(t *testing.T) {
	// struct sg_io_hdr is 88 bytes on 64-bit Linux and 64 bytes on 32-bit.
	// The last field, info, is at offset 80 or 60; 64-bit adds 4 bytes
	// of padding at the end.
	size, info := uintptr(64), uintptr(60)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		size, info = 88, 80
	}
	if got := unsafe.Sizeof(sgIOHdr{}); got != size {
		t.Fatalf("sg_io_hdr size %d, want %d", got, size)
	}
	if off := unsafe.Offsetof(sgIOHdr{}.info); off != info {
		t.Fatalf("info offset %d, want %d", off, info)
	}
	if off := unsafe.Offsetof(sgIOHdr{}.usrPtr); off != 56 && size == 88 {
		t.Fatalf("usr_ptr offset %d", off)
	}
}

func TestOpenRejectsNonSG(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o644)
	if _, err := Open(file); err == nil || !strings.Contains(err.Error(), "not a character device") {
		t.Fatalf("regular file: %v", err)
	}
	if _, err := Open("/dev/null"); err == nil || !strings.Contains(err.Error(), "not a SCSI generic device") {
		t.Fatalf("/dev/null: %v", err)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing device opened")
	}
}

func TestDoRejectsBadArguments(t *testing.T) {
	d := &sgDevice{path: "x"}
	if _, err := d.Do(nil, DirNone, nil, shortTimeout); err == nil {
		t.Fatal("empty CDB accepted")
	}
	if _, err := d.Do(make([]byte, 17), DirNone, nil, shortTimeout); err == nil {
		t.Fatal("17-byte CDB accepted")
	}
	if _, err := d.Do(make([]byte, 6), DirNone, make([]byte, 1), shortTimeout); err == nil {
		t.Fatal("buffer without direction accepted")
	}
}

// TestRealDrive runs the read-only queries against a real drive when
// TAPEMGR_TEST_DEVICE names one, for example /dev/sg2. It never moves
// media or changes drive settings.
func TestRealDrive(t *testing.T) {
	path := os.Getenv("TAPEMGR_TEST_DEVICE")
	if path == "" {
		t.Skip("set TAPEMGR_TEST_DEVICE to test against a real drive")
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	info, err := Gather(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", info)
	if info.Inquiry.Vendor == "" || len(info.Problems) != 0 {
		t.Fatalf("%+v", info)
	}
	if info.Cartridge != nil {
		wp, err := WriteProtected(d)
		if err != nil {
			t.Fatal(err)
		}
		if info.VHF != nil && info.VHF.WriteProtect != wp {
			t.Fatalf("MODE SENSE says write protected %v, VHF says %v", wp, info.VHF.WriteProtect)
		}
		t.Logf("write protected: %v", wp)
	}
}
