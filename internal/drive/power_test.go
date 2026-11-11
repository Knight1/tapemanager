package drive_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/drive/drivetest"
)

func TestPowerConditionRoundTrip(t *testing.T) {
	f := drivetest.New()
	f.StandbyEnabled, f.StandbyTimer = true, 300 // 30 s, in 100 ms units
	pc, err := drive.ReadPowerCondition(f)
	if err != nil {
		t.Fatal(err)
	}
	if pc.IdleEnabled || !pc.StandbyEnabled || pc.StandbyTimer != 30*time.Second {
		t.Fatalf("read %+v", pc)
	}
	if !pc.HaveSettable || !pc.IdleSettable || !pc.StandbySettable {
		t.Fatalf("settable %+v", pc)
	}
	// Enable idle at 2 minutes, turn standby off, write it back.
	pc.IdleEnabled, pc.IdleTimer = true, 2*time.Minute
	pc.StandbyEnabled = false
	if err := drive.SetPowerCondition(f, pc); err != nil {
		t.Fatal(err)
	}
	if !f.IdleEnabled || f.IdleTimer != 1200 || f.StandbyEnabled {
		t.Fatalf("after set: idle=%v timer=%d standby=%v", f.IdleEnabled, f.IdleTimer, f.StandbyEnabled)
	}
	pc2, err := drive.ReadPowerCondition(f)
	if err != nil || !pc2.IdleEnabled || pc2.IdleTimer != 2*time.Minute || pc2.StandbyEnabled {
		t.Fatalf("reread %+v %v", pc2, err)
	}
}

// A drive that does not allow the idle timer reports it as not settable and
// setting it is refused with a clear error, not the drive's raw rejection.
func TestPowerConditionNotSettable(t *testing.T) {
	f := drivetest.New()
	f.IdleNotSettable = true
	pc, err := drive.ReadPowerCondition(f)
	if err != nil {
		t.Fatal(err)
	}
	if !pc.HaveSettable || pc.IdleSettable || !pc.StandbySettable {
		t.Fatalf("settable %+v", pc)
	}
	pc.IdleEnabled, pc.IdleTimer = true, 5*time.Minute
	err = drive.SetPowerCondition(f, pc)
	if err == nil || !strings.Contains(err.Error(), "does not allow setting the idle") {
		t.Fatalf("expected a clear refusal, got %v", err)
	}
	if f.IdleEnabled { // nothing was written
		t.Fatal("wrote a non-settable timer")
	}
	// Standby is still settable on the same drive.
	pc.IdleEnabled = false
	pc.StandbyEnabled, pc.StandbyTimer = true, 30*time.Second
	if err := drive.SetPowerCondition(f, pc); err != nil {
		t.Fatalf("standby: %v", err)
	}
	if !f.StandbyEnabled || f.StandbyTimer != 300 {
		t.Fatalf("standby not set: %v %d", f.StandbyEnabled, f.StandbyTimer)
	}
}

// VerifyPowerCondition confirms the drive kept what was asked, and reports a
// clear mismatch (timer or enable bit) when it did not.
func TestVerifyPowerCondition(t *testing.T) {
	f := drivetest.New()
	f.IdleEnabled, f.IdleTimer = true, 1200 // 2 minutes, in 100 ms units
	pc, err := drive.ReadPowerCondition(f)
	if err != nil {
		t.Fatal(err)
	}
	// The drive holds exactly what pc describes: verification passes.
	if err := drive.VerifyPowerCondition(f, pc); err != nil {
		t.Fatalf("expected a match, got %v", err)
	}
	// A timer the drive does not hold is a clear mismatch.
	pc.IdleTimer = 5 * time.Minute
	if err := drive.VerifyPowerCondition(f, pc); err == nil || !strings.Contains(err.Error(), "different idle timer") {
		t.Fatalf("expected an idle timer mismatch, got %v", err)
	}
	// So is an enable bit the drive did not store.
	pc.IdleTimer = 2 * time.Minute
	pc.StandbyEnabled = true
	if err := drive.VerifyPowerCondition(f, pc); err == nil || !strings.Contains(err.Error(), "store the standby") {
		t.Fatalf("expected a standby state mismatch, got %v", err)
	}
}

// A drive that allows neither timer (like the IBM LTO-6) reports both as not
// settable, and any set is refused with the "manages power itself" message.
func TestPowerConditionNoneSettable(t *testing.T) {
	f := drivetest.New()
	f.IdleNotSettable, f.StandbyNotSettable = true, true
	pc, err := drive.ReadPowerCondition(f)
	if err != nil {
		t.Fatal(err)
	}
	if !pc.HaveSettable || pc.IdleSettable || pc.StandbySettable {
		t.Fatalf("settable %+v", pc)
	}
	pc.StandbyEnabled, pc.StandbyTimer = true, 5*time.Minute
	if err := drive.SetPowerCondition(f, pc); err == nil || !strings.Contains(err.Error(), "manages power itself") {
		t.Fatalf("expected the manages-power refusal, got %v", err)
	}
	if f.StandbyEnabled {
		t.Fatal("wrote to a drive that allows nothing")
	}
}
