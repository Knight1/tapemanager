package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/drive/drivetest"
)

func fastSelfTest(t *testing.T) {
	t.Helper()
	oldWait, oldInterval := selfTestWait, selfTestInterval
	selfTestWait, selfTestInterval = time.Second, time.Millisecond
	t.Cleanup(func() { selfTestWait, selfTestInterval = oldWait, oldInterval })
}

func TestCLIDriveSelfTestPasses(t *testing.T) {
	fastSelfTest(t)
	f := drivetest.New()
	f.SelfTestRuns = 1
	useFake(t, f, fakeFound, nil)
	code, out, _ := runCmd(t, "drive", "selftest")
	if code != 0 || !strings.Contains(out, "running its short self-test") || !strings.Contains(out, "Self-test passed") {
		t.Fatalf("%d %s", code, out)
	}
	if f.LastDiag != 0x20 {
		t.Fatalf("diag %#x", f.LastDiag)
	}
}

func TestCLIDriveSelfTestFails(t *testing.T) {
	fastSelfTest(t)
	f := drivetest.New()
	f.SelfTestFail = true
	useFake(t, f, fakeFound, nil)
	code, out, _ := runCmd(t, "drive", "selftest", "--extended")
	if code != exitFailure || !strings.Contains(out, "running its extended self-test") || !strings.Contains(out, "Self-test FAILED") || !strings.Contains(out, "sense 4/44/00") {
		t.Fatalf("%d %s", code, out)
	}
	if f.LastDiag != 0x40 {
		t.Fatalf("diag %#x", f.LastDiag)
	}
}

func TestCLIDriveSelfTestStatus(t *testing.T) {
	f := drivetest.New() // nothing has run
	useFake(t, f, fakeFound, nil)
	code, out, _ := runCmd(t, "drive", "selftest", "--status")
	if code != 0 || !strings.Contains(out, "no self-test result yet") {
		t.Fatalf("%d %s", code, out)
	}
	// A status check must not start a test.
	for _, op := range f.Commands {
		if op == 0x1D {
			t.Fatal("status started a self-test")
		}
	}
}

func TestCLIDriveDensity(t *testing.T) {
	f := drivetest.New()
	f.Compressing = true
	useFake(t, f, fakeFound, nil)
	code, out, _ := runCmd(t, "drive", "density")
	for _, want := range []string{
		"recording formats the drive supports",
		"LTO-6", "density 0x5a, read and write, default",
		"LTO-4", "density 0x46, read only",
		"(LTO6 2500G)", "Hardware compression: on",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if code != 0 || t.Failed() {
		t.Fatalf("code %d", code)
	}
}

func TestCLIDriveDensityCompressionOff(t *testing.T) {
	useFake(t, drivetest.New(), fakeFound, nil)
	code, out, _ := runCmd(t, "drive", "density")
	if code != 0 || !strings.Contains(out, "Hardware compression: off") {
		t.Fatalf("%d %s", code, out)
	}
}
