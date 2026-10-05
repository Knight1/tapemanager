package drive_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/drive/drivetest"
)

func image(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func TestUpdateFirmware(t *testing.T) {
	f := drivetest.New()
	img := image(1<<20 + 12345)
	f.ImageSize, f.NewRevision = len(img), "F1A0"
	var calls []int
	if err := drive.UpdateFirmware(f, img, 256<<10, func(done, total int) { calls = append(calls, done) }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.Received, img) || f.Downloads != 5 || calls[len(calls)-1] != len(img) {
		t.Fatalf("received %d bytes in %d pieces, progress %v", len(f.Received), f.Downloads, calls)
	}
	q, err := drive.WaitForDrive(func() (drive.Device, error) { return f, nil }, time.Second, time.Millisecond)
	if err != nil || q.Revision != "F1A0" {
		t.Fatalf("%+v %v", q, err)
	}
}

func TestUpdateFirmwareAlignment(t *testing.T) {
	f := drivetest.New()
	f.Boundary = 9 // 512-byte offsets
	img := image(10000)
	f.ImageSize = len(img)
	if err := drive.UpdateFirmware(f, img, 1000, nil); err != nil {
		t.Fatal(err)
	}
	// 1000 is rounded down to 512.
	if f.Downloads != 20 || !bytes.Equal(f.Received, img) {
		t.Fatalf("%d pieces", f.Downloads)
	}
	if c, err := drive.CheckFirmwareImage(drive.MicrocodeBuffer{Boundary: 12, Capacity: 1 << 20}, img, 100); err != nil || c != 4096 {
		t.Fatalf("%d %v", c, err)
	}
}

func TestUpdateFirmwareRefuses(t *testing.T) {
	f := drivetest.New()
	if err := drive.UpdateFirmware(f, image(1<<24), 0, nil); err == nil || !strings.Contains(err.Error(), "larger than WRITE BUFFER can address") {
		t.Fatalf("%v", err)
	}
	if err := drive.UpdateFirmware(f, nil, 0, nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("%v", err)
	}
	if f.Downloads != 0 {
		t.Fatal("download started")
	}
	if _, err := drive.CheckFirmwareImage(drive.MicrocodeBuffer{Boundary: 30, Capacity: 1 << 20}, image(10), 0); err == nil {
		t.Error("unsupported alignment accepted")
	}
	f = drivetest.New()
	f.Fail = map[byte]*drive.CommandError{0x3C: {Op: 0x3C, Status: 2, Key: drive.SenseIllegalRequest, ASC: 0x24}}
	if err := drive.UpdateFirmware(f, image(10), 0, nil); err == nil || !strings.Contains(err.Error(), "microcode buffer") {
		t.Fatalf("%v", err)
	}
}

// A download that fails partway reports where; the drive keeps its old
// firmware because the image is incomplete.
func TestUpdateFirmwareInterrupted(t *testing.T) {
	f := drivetest.New()
	img := image(1 << 20)
	f.ImageSize, f.NewRevision, f.FailAt = len(img), "F1A0", 512<<10
	err := drive.UpdateFirmware(f, img, 256<<10, nil)
	var fe *drive.FirmwareError
	if !errors.As(err, &fe) || fe.Offset != 512<<10 {
		t.Fatalf("%v", err)
	}
	if q, _ := drive.ReadInquiry(f); q.Revision != "E6R3" {
		t.Fatalf("revision changed to %s", q.Revision)
	}
}

func TestWaitForDriveTimeout(t *testing.T) {
	_, err := drive.WaitForDrive(func() (drive.Device, error) { return nil, errors.New("gone") }, 10*time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("%v", err)
	}
	f := drivetest.New()
	f.Fail = map[byte]*drive.CommandError{0x12: {Op: 0x12, Status: 2, Key: drive.SenseNotReady, ASC: 0x04, ASCQ: 0x01}}
	if _, err := drive.WaitForDrive(func() (drive.Device, error) { return f, nil }, 10*time.Millisecond, time.Millisecond); err == nil || !strings.Contains(err.Error(), "becoming ready") {
		t.Fatalf("%v", err)
	}
}

func TestFirmwareBuild(t *testing.T) {
	f := drivetest.New()
	q, _ := drive.ReadInquiry(f)
	fb, err := drive.ReadFirmwareBuild(f, q)
	if err != nil || fb != (drive.FirmwareBuild{Name: "LTO6_E6R3", Built: "2014-08-08 13:08:08", Platform: "sas_hh"}) {
		t.Fatalf("%+v %v", fb, err)
	}
	q.Vendor = "HP"
	if _, err := drive.ReadFirmwareBuild(f, q); !errors.Is(err, drive.ErrUnsupported) {
		t.Fatalf("other vendor: %v", err)
	}
	info, _ := drive.Gather(f)
	if info.Firmware.Name != "LTO6_E6R3" {
		t.Fatalf("%+v", info.Firmware)
	}
}

// The drive reports 5 MiB for buffer 0, but IBM LTO-6 images are larger and
// accepted: the buffer capacity is no limit for microcode.
func TestFirmwareLargerThanBufferDescriptor(t *testing.T) {
	f := drivetest.New()
	f.BufferCapacity = 5 << 20
	img := image(6 << 20)
	f.ImageSize = len(img)
	if err := drive.UpdateFirmware(f, img, 0, nil); err != nil || !bytes.Equal(f.Received, img) {
		t.Fatalf("%v", err)
	}
}

func TestCheckIBMImage(t *testing.T) {
	f := drivetest.New()
	q, _ := drive.ReadInquiry(f)
	img := drivetest.IBMImage(drivetest.FakeLoadID, drivetest.FakeModelID, "E6R4", 200<<10, nil)
	got, err := drive.CheckIBMImage(f, q, img)
	if err != nil || got.Level != "E6R4" || got.ModelID != "TESTID01" || got.Built != "2026/01/02" {
		t.Fatalf("%+v %v", got, err)
	}
	other := drivetest.IBMImage([]byte{9, 9, 9, 9}, drivetest.FakeModelID, "E6R4", 200<<10, nil)
	if _, err := drive.CheckIBMImage(f, q, other); err == nil || !strings.Contains(err.Error(), "another drive model") {
		t.Fatalf("other load ID: %v", err)
	}
	otherModel := drivetest.IBMImage(drivetest.FakeLoadID, []byte{0xC1, 0xC2, 0xC3, 0xC4, 0xF1, 0xF2, 0xF3, 0xF4}, "E6R4", 200<<10, nil)
	if _, err := drive.CheckIBMImage(f, q, otherModel); err == nil || !strings.Contains(err.Error(), "image ABCD1234") {
		t.Fatalf("other model: %v", err)
	}
	if _, err := drive.CheckIBMImage(f, q, image(200<<10)); !errors.Is(err, drive.ErrNotIBMImage) {
		t.Fatalf("plain file: %v", err)
	}
	if _, err := drive.CheckIBMImage(f, q, img[:len(img)-1]); err == nil || !strings.Contains(err.Error(), "truncated or damaged") {
		t.Fatalf("truncated: %v", err)
	}
	// Other vendors: no check here.
	q.Vendor = "HP"
	if got, err := drive.CheckIBMImage(f, q, image(10)); got != nil || err != nil {
		t.Fatalf("%+v %v", got, err)
	}
	q.Vendor = "IBM"
	f.Fail = map[byte]*drive.CommandError{0x12: {Op: 0x12, Status: 2, Key: drive.SenseIllegalRequest, ASC: 0x24}}
	if _, err := drive.CheckIBMImage(f, q, img); err == nil || !strings.Contains(err.Error(), "VPD 0x03") {
		t.Fatalf("no VPD: %v", err)
	}
}

func TestParseIBMImageShort(t *testing.T) {
	for _, n := range []int{0, 0x27} {
		if _, err := drive.ParseIBMImage(make([]byte, n)); !errors.Is(err, drive.ErrNotIBMImage) {
			t.Fatalf("%d: %v", n, err)
		}
	}
}
