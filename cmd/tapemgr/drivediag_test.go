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

func TestCLIDrivePowerShow(t *testing.T) {
	f := drivetest.New()
	f.IdleEnabled, f.IdleTimer = true, 1200 // 2 minutes
	useFake(t, f, fakeFound, nil)
	code, out, _ := runCmd(t, "drive", "power")
	if code != 0 || !strings.Contains(out, "Idle:") || !strings.Contains(out, "after 2m0s idle") || !strings.Contains(out, "Standby:") || !strings.Contains(out, "off") {
		t.Fatalf("%d %s", code, out)
	}
	// A plain show sends no MODE SELECT.
	for _, op := range f.Commands {
		if op == 0x15 {
			t.Fatal("show wrote a mode page")
		}
	}
}

func TestCLIDrivePowerDryRunAndApply(t *testing.T) {
	f := drivetest.New()
	useFake(t, f, fakeFound, nil)
	// Without --yes the change is shown but not applied.
	code, out, _ := runCmd(t, "drive", "power", "--standby-after", "30s")
	if code != 0 || !strings.Contains(out, "After the change") || !strings.Contains(out, "Nothing was changed") || f.StandbyEnabled {
		t.Fatalf("dry run: %d %s", code, out)
	}
	// With --yes it is written to the drive.
	code, out, _ = runCmd(t, "drive", "power", "--standby-after", "30s", "--yes")
	if code != 0 || !strings.Contains(out, "Applied") || !f.StandbyEnabled || f.StandbyTimer != 300 {
		t.Fatalf("apply: %d %s standby=%v %d", code, out, f.StandbyEnabled, f.StandbyTimer)
	}
	// --off turns the timers off.
	code, out, _ = runCmd(t, "drive", "power", "--off", "--yes")
	if code != 0 || f.StandbyEnabled || f.IdleEnabled {
		t.Fatalf("off: %d %s", code, out)
	}
}

func TestCLIDrivePowerNotSettable(t *testing.T) {
	f := drivetest.New()
	f.IdleNotSettable = true
	useFake(t, f, fakeFound, nil)
	// The read marks idle as not settable on this drive.
	code, out, _ := runCmd(t, "drive", "power")
	if code != 0 || !strings.Contains(out, "not settable on this drive") {
		t.Fatalf("show: %d %s", code, out)
	}
	// Setting it is refused with a clear message (on stderr), nothing is
	// written, and no misleading "After the change" preview is shown.
	code, out, errOut := runCmd(t, "drive", "power", "--idle-after", "5m", "--yes")
	if code != exitFailure || !strings.Contains(errOut, "does not allow setting the idle") || f.IdleEnabled || strings.Contains(out, "After the change") {
		t.Fatalf("set idle: %d out=%q err=%q idle=%v", code, out, errOut, f.IdleEnabled)
	}
}

// The IBM LTO-6 allows neither timer: both read as not settable (with a
// plain-language note), and any set is refused.
func TestCLIDrivePowerNoneSettable(t *testing.T) {
	f := drivetest.New()
	f.IdleNotSettable, f.StandbyNotSettable = true, true
	useFake(t, f, fakeFound, nil)
	code, out, _ := runCmd(t, "drive", "power")
	if code != 0 || strings.Count(out, "not settable on this drive") != 2 || !strings.Contains(out, "manages power itself") {
		t.Fatalf("show: %d %s", code, out)
	}
	code, _, errOut := runCmd(t, "drive", "power", "--standby-after", "5m", "--yes")
	if code != exitFailure || !strings.Contains(errOut, "manages power itself") || f.StandbyEnabled {
		t.Fatalf("set: %d %s standby=%v", code, errOut, f.StandbyEnabled)
	}
}
