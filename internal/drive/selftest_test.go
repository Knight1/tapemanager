package drive_test

import (
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/drive/drivetest"
)

func TestSelfTestPasses(t *testing.T) {
	f := drivetest.New()
	f.SelfTestRuns = 2 // in progress for two reads, then passes
	if err := drive.StartSelfTest(f, drive.SelfTestShort); err != nil {
		t.Fatal(err)
	}
	if f.LastDiag != 0x20 { // background short
		t.Fatalf("diag byte %#x", f.LastDiag)
	}
	r, ok, err := drive.PollSelfTest(f, time.Second, time.Millisecond)
	if err != nil || !ok || r.InProgress() || !r.Passed() {
		t.Fatalf("%+v ok=%v err=%v", r, ok, err)
	}
	if r.Description() != "passed" {
		t.Fatalf("description %q", r.Description())
	}
}

func TestSelfTestFails(t *testing.T) {
	f := drivetest.New()
	f.SelfTestFail = true
	if err := drive.StartSelfTest(f, drive.SelfTestExtended); err != nil {
		t.Fatal(err)
	}
	if f.LastDiag != 0x40 { // background extended
		t.Fatalf("diag byte %#x", f.LastDiag)
	}
	r, ok, err := drive.PollSelfTest(f, time.Second, time.Millisecond)
	if err != nil || !ok || r.Passed() {
		t.Fatalf("%+v ok=%v err=%v", r, ok, err)
	}
	if r.Key != drive.SenseHardwareError || r.ASC != 0x44 || r.Segment != 7 {
		t.Fatalf("codes %x/%02x/%02x seg %d", r.Key, r.ASC, r.ASCQ, r.Segment)
	}
}

func TestSelfTestNoResult(t *testing.T) {
	f := drivetest.New() // no self-test has run
	if r, ok, err := drive.ReadSelfTest(f); err != nil || ok {
		t.Fatalf("%+v ok=%v err=%v", r, ok, err)
	}
}

func TestSelfTestTimeout(t *testing.T) {
	f := drivetest.New()
	f.SelfTestRuns = 1 << 30 // never completes
	if err := drive.StartSelfTest(f, drive.SelfTestShort); err != nil {
		t.Fatal(err)
	}
	if _, _, err := drive.PollSelfTest(f, 5*time.Millisecond, time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}
}
