package drive_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/drive/drivetest"
)

func TestGather(t *testing.T) {
	f := drivetest.New()
	info, err := drive.Gather(f)
	if err != nil {
		t.Fatal(err)
	}
	if info.Inquiry.Product != "ULT3580-HH6" || info.Serial != "0000000001" || info.Ready != nil || info.Path != f.DevPath {
		t.Fatalf("%+v", info)
	}
	if info.VHF == nil || !info.VHF.Mounted || info.VHF.CleanRequested {
		t.Fatalf("%+v", info.VHF)
	}
	c := info.Cartridge
	if c == nil {
		t.Fatal("no cartridge")
	}
	if c.Serial != "0000000002" || c.Format != "LTO-6" || c.Kind != "data" || c.Manufacturer != "QUANTUM" ||
		c.ManufactureDate != "20200101" || c.LoadCount != 3 || c.WrittenMiB != 20 || c.ReadMiB != 30 ||
		c.Application != "IBM LTFS 2.4.9.0" || c.LTFSVolume != f.LTFSUUID || c.Barcode != "" {
		t.Fatalf("%+v", c)
	}
	if len(c.Partitions) != 2 || c.Partitions[1].MaximumMiB != 2300010 || c.Partitions[0].RemainingMiB != 35000 {
		t.Fatalf("%+v", c.Partitions)
	}
	if c.Stats == nil || c.Stats.WriteRetries.N != 1 || !c.Stats.WriteRetries.OK {
		t.Fatalf("%+v", c.Stats)
	}
	if info.WriteErrors == nil || info.WriteErrors.Corrected != 1 || info.ReadErrors == nil {
		t.Fatalf("%+v %+v", info.WriteErrors, info.ReadErrors)
	}
	if len(info.Alerts) != 0 || len(info.Problems) != 0 || len(info.Warnings()) != 0 {
		t.Fatalf("%v %v %v", info.Alerts, info.Problems, info.Warnings())
	}
	// Gather only reads: nothing that moves media was sent.
	if slices.Contains(f.Commands, 0x1B) {
		t.Fatal("load/unload sent while gathering")
	}
}

func TestGatherNoCartridge(t *testing.T) {
	f := drivetest.New()
	f.NoMedium = true
	info, err := drive.Gather(f)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(info.Ready, drive.ErrNoMedium) || info.Cartridge != nil || len(info.Problems) != 0 {
		t.Fatalf("%+v", info)
	}
	if slices.Contains(f.Commands, 0x8C) {
		t.Fatal("cartridge memory read without a cartridge")
	}
}

// After an eject the cartridge stays in the slot. The drive then reports
// "load needed", and reading the cartridge memory fails with a medium
// error that must not be reported as a problem.
func TestGatherEjectedInSlot(t *testing.T) {
	f := drivetest.New()
	f.InSlot = true
	info, err := drive.Gather(f)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(info.Ready, drive.ErrNotLoaded) || info.Cartridge != nil || len(info.Problems) != 0 {
		t.Fatalf("%+v", info)
	}
	if slices.Contains(f.Commands, 0x8C) {
		t.Fatal("cartridge memory read while unloaded")
	}
	if err := drive.Load(f); err != nil || f.InSlot || drive.TestUnitReady(f) != nil {
		t.Fatalf("load: %v", err)
	}
}

func TestWarnings(t *testing.T) {
	f := drivetest.New()
	f.CleanRequested = true
	f.Alerts = []int{10, 20, 3}
	f.WriteUncorr, f.ReadUncorr, f.VolumeReadErrs = 2, 3, 4
	info, err := drive.Gather(f)
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(info.Warnings(), "\n")
	for _, want := range []string{"requests cleaning", "TapeAlert 3 (warning)", "TapeAlert 20 (critical)", "2 uncorrected write", "3 uncorrected read", "4 unrecovered read errors over its life"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing %q in\n%s", want, w)
		}
	}
	// Informational flags are shown by info but are not warnings.
	if strings.Contains(w, "TapeAlert 10") || len(info.Alerts) != 3 {
		t.Fatalf("%s %v", w, info.Alerts)
	}

	f = drivetest.New()
	f.CleanRequested, f.CleanRequired = true, true
	info, _ = drive.Gather(f)
	if w := info.Warnings(); len(w) != 1 || !strings.Contains(w[0], "needs cleaning now") {
		t.Fatalf("%v", w)
	}
}

func TestGatherDegrades(t *testing.T) {
	f := drivetest.New()
	f.Fail = map[byte]*drive.CommandError{
		0x8C: {Op: 0x8C, Status: 2, Key: drive.SenseHardwareError, ASC: 0x44},
	}
	info, err := drive.Gather(f)
	if err != nil {
		t.Fatal(err)
	}
	if info.Cartridge != nil || len(info.Problems) != 1 || !strings.Contains(info.Problems[0], "cartridge memory") {
		t.Fatalf("%+v", info.Problems)
	}

	// Pages the drive does not support are skipped silently; other
	// failures are problems.
	f = drivetest.New()
	f.Fail = map[byte]*drive.CommandError{0x4D: {Op: 0x4D, Status: 2, Key: drive.SenseIllegalRequest, ASC: 0x24}}
	info, _ = drive.Gather(f)
	if len(info.Problems) != 0 || info.VHF != nil || info.WriteErrors != nil {
		t.Fatalf("%+v", info)
	}
	f.Fail[0x4D] = &drive.CommandError{Op: 0x4D, Status: 2, Key: drive.SenseHardwareError}
	info, _ = drive.Gather(f)
	if len(info.Problems) != 9 {
		t.Fatalf("%v", info.Problems)
	}
}

func TestGatherRejectsNonTape(t *testing.T) {
	f := drivetest.New()
	f.DeviceType = 0
	if _, err := drive.Gather(f); err == nil || !strings.Contains(err.Error(), "not a tape drive") {
		t.Fatalf("%v", err)
	}
	f = drivetest.New()
	f.Fail = map[byte]*drive.CommandError{0x12: {Op: 0x12, Status: 2, Key: drive.SenseHardwareError}}
	if _, err := drive.Gather(f); err == nil {
		t.Fatal("inquiry failure not reported")
	}
}

// unitAttention reports a cartridge change once, like a drive after a load.
type unitAttention struct {
	*drivetest.Fake
	pending int
}

func (u *unitAttention) Do(cdb []byte, dir drive.Direction, buf []byte, timeout time.Duration) (int, error) {
	if cdb[0] == 0x00 && u.pending > 0 {
		u.pending--
		return 0, &drive.CommandError{Op: 0, Status: 2, Key: drive.SenseUnitAttention, ASC: 0x28}
	}
	return u.Fake.Do(cdb, dir, buf, timeout)
}

func TestTestUnitReadyRetriesUnitAttention(t *testing.T) {
	u := &unitAttention{Fake: drivetest.New(), pending: 2}
	if err := drive.TestUnitReady(u); err != nil {
		t.Fatal(err)
	}
	u.pending = 5
	if err := drive.TestUnitReady(u); err == nil {
		t.Fatal("endless unit attention not reported")
	}
}

func TestLoadUnload(t *testing.T) {
	f := drivetest.New()
	if err := drive.Unload(f); err != nil || f.Unloaded != 1 {
		t.Fatalf("%v %d", err, f.Unloaded)
	}
	if err := drive.Unload(f); !errors.Is(err, drive.ErrNoMedium) {
		t.Fatalf("%v", err)
	}
	if err := drive.Load(f); err != nil || f.Loaded != 1 {
		t.Fatalf("%v", err)
	}
	f.PreventRemoval = true
	if err := drive.Unload(f); !errors.Is(err, drive.ErrRemovalPrevented) {
		t.Fatalf("%v", err)
	}
}

// shortModeSense answers MODE SENSE with fewer bytes than a header.
type shortModeSense struct{ *drivetest.Fake }

func (s shortModeSense) Do(cdb []byte, dir drive.Direction, buf []byte, timeout time.Duration) (int, error) {
	if cdb[0] == 0x1A {
		return copy(buf, []byte{1, 0}), nil
	}
	return s.Fake.Do(cdb, dir, buf, timeout)
}

func TestWriteProtected(t *testing.T) {
	f := drivetest.New()
	if wp, err := drive.WriteProtected(f); err != nil || wp {
		t.Fatalf("%v %v", wp, err)
	}
	f.WriteProtect = true
	if wp, err := drive.WriteProtected(f); err != nil || !wp {
		t.Fatalf("%v %v", wp, err)
	}
	// Without MODE SENSE the VHF data answers.
	f.Fail = map[byte]*drive.CommandError{0x1A: {Op: 0x1A, Status: 2, Key: drive.SenseIllegalRequest, ASC: 0x20}}
	if wp, err := drive.WriteProtected(f); err != nil || !wp {
		t.Fatalf("VHF fallback: %v %v", wp, err)
	}
	// Neither available: an error, not a guess.
	f.Fail[0x4D] = &drive.CommandError{Op: 0x4D, Status: 2, Key: drive.SenseIllegalRequest, ASC: 0x20}
	if _, err := drive.WriteProtected(f); err == nil {
		t.Fatal("no source reported as not protected")
	}
	f = drivetest.New()
	f.NoMedium = true
	if _, err := drive.WriteProtected(f); !errors.Is(err, drive.ErrNoMedium) {
		t.Fatalf("%v", err)
	}
	if _, err := drive.WriteProtected(shortModeSense{drivetest.New()}); err == nil {
		t.Fatal("short header accepted")
	}
}

func TestGatherEncryption(t *testing.T) {
	f := drivetest.New()
	info, err := drive.Gather(f)
	if err != nil || info.Encryption == nil || info.Encryption.Encrypting() || len(info.Algorithms) != 3 {
		t.Fatalf("%+v %v", info, err)
	}
	f.Encrypting = true
	f.KeyID = []byte{'T', 'M', 'G', 9, 8, 7, 6, 5, 4, 3, 2, 1}
	info, _ = drive.Gather(f)
	if !info.Encryption.Encrypting() || info.Encryption.KeyName() != "TMG090807060504030201" || info.EncryptionAlgorithm() != "AES-256-GCM" {
		t.Fatalf("%+v", info.Encryption)
	}
	// A drive without encryption is not a problem.
	f = drivetest.New()
	f.NoEncryption = true
	info, _ = drive.Gather(f)
	if info.Encryption != nil || len(info.Problems) != 0 || info.EncryptionAlgorithm() != "" {
		t.Fatalf("%+v", info)
	}
	// A failing query is.
	f = drivetest.New()
	f.Fail = map[byte]*drive.CommandError{0xA2: {Op: 0xA2, Status: 2, Key: drive.SenseHardwareError}}
	info, _ = drive.Gather(f)
	if len(info.Problems) != 1 || !strings.Contains(info.Problems[0], "encryption") {
		t.Fatalf("%v", info.Problems)
	}
}
