package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/drive/drivetest"
	"github.com/Knight1/tapemanager/internal/manifest"
)

func TestMain(m *testing.M) {
	// Tests never talk to a real drive.
	driveWarnings = func(string) []string { return nil }
	checkWriteProtect = func(string) error { return nil }
	driveEncryption = func(string) (*drive.EncryptionStatus, error) { return nil, errors.New("no drive in tests") }
	openDevice = func(string) (drive.Device, error) { return nil, errors.New("no drive in tests") }
	os.Exit(m.Run())
}

// useFake routes drive commands to a fake drive with the given mounts.
func useFake(t *testing.T, f *drivetest.Fake, found drive.Found, mounts []string) {
	t.Helper()
	oldOpen, oldResolve, oldList, oldMounts, oldLTFS := openDevice, resolveDrive, listDrives, mountPoints, ltfsMounts
	t.Cleanup(func() {
		openDevice, resolveDrive, listDrives, mountPoints, ltfsMounts = oldOpen, oldResolve, oldList, oldMounts, oldLTFS
	})
	openDevice = func(path string) (drive.Device, error) {
		if path != found.SG {
			return nil, errors.New("unexpected device " + path)
		}
		return f, nil
	}
	resolveDrive = func(path, tape string) (drive.Found, error) { return found, nil }
	listDrives = func() ([]drive.Found, error) { return []drive.Found{found}, nil }
	mountPoints = func(drive.Found, string) ([]string, error) { return mounts, nil }
	ltfsMounts = func() ([]string, error) { return mounts, nil }
}

var fakeFound = drive.Found{SG: "/dev/sg9", Names: []string{"st0", "nst0"}, Vendor: "IBM", Model: "ULT3580-HH6", Rev: "E6R3"}

func TestCLIDriveList(t *testing.T) {
	useFake(t, drivetest.New(), fakeFound, []string{"/mnt/ltfs"})
	code, out, _ := runCmd(t, "drive", "list")
	if code != 0 || !strings.Contains(out, "/dev/sg9") || !strings.Contains(out, "ULT3580-HH6") || !strings.Contains(out, "mounted at /mnt/ltfs") {
		t.Fatalf("%d %s", code, out)
	}
	listDrives = func() ([]drive.Found, error) { return nil, nil }
	if code, out, _ := runCmd(t, "drive", "list"); code != 0 || !strings.Contains(out, "No tape drives") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCLIDriveInfo(t *testing.T) {
	f := drivetest.New()
	f.CleanRequested = true
	f.Alerts = []int{20}
	useFake(t, f, fakeFound, nil)

	// The catalog knows the loaded cartridge by its LTFS volume UUID.
	cat, tapeDir := t.TempDir(), t.TempDir()
	tp, err := manifest.Open(tapeDir)
	if err != nil {
		t.Fatal(err)
	}
	vol, err := tp.InitVolume("T7", f.LTFSUUID)
	tp.Close()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := catalog.Open(cat)
	if _, err := c.Import(tapeDir); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCmd(t, "drive", "info", "--catalog", cat)
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	for _, want := range []string{
		"IBM ULT3580-HH6, firmware E6R3, serial 0000000001 (/dev/sg9)",
		"Firmware:      LTO6_E6R3, built 2014-08-08 13:08:08, sas_hh",
		"Status:        ready, idle",
		"Cleaning:      REQUESTED; 30 cleanings so far, 100 tape hours since the last one",
		"Compression:   on; since the cartridge was loaded: written 2.50:1",
		"Powered on:    40000 hours (4.6 years), 20 power cycles",
		"1000 cartridge loads, 5000 hours of tape motion, 60000 km of tape",
		"tape motion by cartridge type: LTO-6 5000 h",
		"lifetime: 0 hard write errors, 1 hard read error; 0 non-medium errors",
		"LTO-6 data, serial 0000000002, barcode -, QUANTUM, made 2020-01-01",
		"Write protect: no",
		"partition 1: 2.2 TiB free of 2.2 TiB",
		"Formatted by:  IBM LTFS 2.4.9.0",
		"catalog tape " + vol.ID,
		"Lifetime use:  3 loads, 3 mounts, 40 full passes",
		"20.0 MiB over its life, 20 MB during the last mount",
		"TapeAlert:     20 (critical)",
		"WARNING:  drive requests cleaning",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if !f.Closed {
		t.Fatal("device not closed")
	}
	// The drive comes first, then the tape, never mixed.
	drivePart, tapePart, ok := strings.Cut(out, "\nTape\n")
	if !ok || !strings.HasPrefix(drivePart, "Drive\n") {
		t.Fatalf("sections:\n%s", out)
	}
	for _, d := range []string{"Model:", "Cleaning:", "Compression:", "Powered on:", "TapeAlert:"} {
		if strings.Contains(tapePart, d) {
			t.Errorf("%s in the tape section", d)
		}
	}
	for _, c := range []string{"Cartridge:", "Write protect:", "LTFS volume:", "Capacity:", "Written:"} {
		if strings.Contains(drivePart, c) {
			t.Errorf("%s in the drive section", c)
		}
	}

	// A missing catalog is not created by info.
	missing := filepath.Join(t.TempDir(), "nocat")
	if code, _, _ := runCmd(t, "drive", "info", "--catalog", missing); code != 0 {
		t.Fatal("info failed without a catalog")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("info created the catalog")
	}

	f2 := drivetest.New()
	f2.NoMedium = true
	useFake(t, f2, fakeFound, nil)
	if code, out, _ := runCmd(t, "drive", "info", "--catalog", missing); code != 0 || !strings.Contains(out, "no cartridge loaded") || strings.Contains(out, "Cartridge:") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCLIDriveCheck(t *testing.T) {
	f := drivetest.New()
	useFake(t, f, fakeFound, nil)
	if code, out, _ := runCmd(t, "drive", "check"); code != 0 || !strings.HasPrefix(out, "OK:") {
		t.Fatalf("%d %s", code, out)
	}
	f.CleanRequired = true
	f.ReadUncorr = 1
	code, out, _ := runCmd(t, "drive", "check")
	if code != exitFailure || !strings.Contains(out, "needs cleaning now") || !strings.Contains(out, "1 uncorrected read") {
		t.Fatalf("%d %s", code, out)
	}
	resolveDrive = func(string, string) (drive.Found, error) { return drive.Found{}, errors.New("no tape drive found") }
	if code, _, errOut := runCmd(t, "drive", "check"); code != exitFailure || !strings.Contains(errOut, "no tape drive") {
		t.Fatalf("%d %s", code, errOut)
	}
}

func TestCLIDriveEject(t *testing.T) {
	f := drivetest.New()
	useFake(t, f, fakeFound, []string{"/mnt/ltfs"})
	code, _, errOut := runCmd(t, "drive", "eject")
	if code != exitFailure || !strings.Contains(errOut, "mounted at /mnt/ltfs") || f.Unloaded != 0 {
		t.Fatalf("mounted: %d %s %d", code, errOut, f.Unloaded)
	}

	// A device unknown to sysfs is refused while any LTFS mount exists.
	unknown := drive.Found{SG: "/dev/sg9"}
	useFake(t, f, unknown, []string{"/mnt/other"})
	mountPoints = func(drive.Found, string) ([]string, error) { return nil, nil }
	if code, _, errOut := runCmd(t, "drive", "eject"); code != exitFailure || !strings.Contains(errOut, "/mnt/other") || f.Unloaded != 0 {
		t.Fatalf("unknown device: %d %s", code, errOut)
	}

	useFake(t, f, fakeFound, nil)
	mountPoints = func(drive.Found, string) ([]string, error) { return nil, errors.New("boom") }
	if code, _, errOut := runCmd(t, "drive", "eject"); code != exitFailure || !strings.Contains(errOut, "checking mounts") || f.Unloaded != 0 {
		t.Fatalf("mount check error: %d %s", code, errOut)
	}

	useFake(t, f, fakeFound, nil)
	f.PreventRemoval = true
	if code, _, errOut := runCmd(t, "drive", "eject"); code != exitFailure || !strings.Contains(errOut, "removal is prevented") {
		t.Fatalf("prevented: %d %s", code, errOut)
	}
	f.PreventRemoval = false
	if code, out, _ := runCmd(t, "drive", "eject"); code != 0 || !strings.Contains(out, "Ejected") || f.Unloaded != 1 {
		t.Fatalf("eject: %d %s", code, out)
	}
	if code, _, errOut := runCmd(t, "drive", "eject"); code != exitFailure || !strings.Contains(errOut, "no cartridge") {
		t.Fatalf("empty drive: %d %s", code, errOut)
	}
}

func TestCLIDriveLoad(t *testing.T) {
	f := drivetest.New()
	f.NoMedium = true
	useFake(t, f, fakeFound, nil)
	if code, out, _ := runCmd(t, "drive", "load"); code != 0 || !strings.Contains(out, "Loaded") || f.Loaded != 1 {
		t.Fatalf("%d %s", code, out)
	}
	f.Fail = map[byte]*drive.CommandError{0x1B: {Op: 0x1B, Status: 2, Key: drive.SenseNotReady, ASC: 0x3A}}
	if code, _, errOut := runCmd(t, "drive", "load"); code != exitFailure || !strings.Contains(errOut, "insert one") {
		t.Fatalf("%d %s", code, errOut)
	}
}

func TestCLIDriveRefusesNonTape(t *testing.T) {
	f := drivetest.New()
	f.DeviceType = 0
	useFake(t, f, fakeFound, nil)
	for _, cmd := range []string{"load", "eject", "info"} {
		if code, _, errOut := runCmd(t, "drive", cmd); code != exitFailure || !strings.Contains(errOut, "not a tape drive") {
			t.Fatalf("%s: %d %s", cmd, code, errOut)
		}
	}
	if f.Loaded != 0 || f.Unloaded != 0 {
		t.Fatal("media moved on a non-tape device")
	}
}

func TestCLIPrintsDriveWarnings(t *testing.T) {
	old := driveWarnings
	t.Cleanup(func() { driveWarnings = old })
	var asked []string
	driveWarnings = func(tape string) []string {
		asked = append(asked, tape)
		return []string{"drive requests cleaning"}
	}
	base := t.TempDir()
	src := filepath.Join(base, "src")
	tape := filepath.Join(base, "tape")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(tape, 0o755)
	os.WriteFile(filepath.Join(src, "a"), []byte("x"), 0o644)
	common := []string{"--tape", tape, "--catalog", filepath.Join(base, "cat"), "--no-mount-check"}
	code, _, errOut := runCmd(t, append(append([]string{"archive", "put"}, common...), src)...)
	if code != 0 || !strings.Contains(errOut, "DRIVE WARNING: drive requests cleaning") {
		t.Fatalf("put: %d %s", code, errOut)
	}
	code, _, errOut = runCmd(t, append([]string{"archive", "verify"}, common...)...)
	if code != 0 || !strings.Contains(errOut, "DRIVE WARNING") {
		t.Fatalf("verify: %d %s", code, errOut)
	}
	if len(asked) != 2 || asked[0] != tape {
		t.Fatalf("%v", asked)
	}
}

func TestDriveWriteProtect(t *testing.T) {
	f := drivetest.New()
	useFake(t, f, fakeFound, nil)
	oldFor := forMount
	t.Cleanup(func() { forMount = oldFor })
	forMount = func(string) (drive.Found, error) { return fakeFound, nil }

	if err := driveWriteProtect("/mnt/ltfs"); err != nil {
		t.Fatalf("writable: %v", err)
	}
	f.WriteProtect = true
	if err := driveWriteProtect("/mnt/ltfs"); err == nil || !strings.Contains(err.Error(), "write protected") {
		t.Fatalf("protected: %v", err)
	}
	if !f.Closed {
		t.Fatal("device not closed")
	}
	// Without a drive to ask, the mount check alone decides.
	forMount = func(string) (drive.Found, error) { return drive.Found{}, errors.New("not a mount point") }
	if err := driveWriteProtect("/mnt/ltfs"); err != nil {
		t.Fatalf("no drive: %v", err)
	}
	forMount = func(string) (drive.Found, error) { return drive.Found{SG: "/dev/other"}, nil }
	if err := driveWriteProtect("/mnt/ltfs"); err != nil {
		t.Fatalf("open error: %v", err)
	}
}

func TestCLIRefusesWriteProtectedTape(t *testing.T) {
	old := checkWriteProtect
	t.Cleanup(func() { checkWriteProtect = old })
	checkWriteProtect = func(string) error { return errors.New("the cartridge in /dev/sg9 is write protected") }
	base := t.TempDir()
	src, tape := filepath.Join(base, "src"), filepath.Join(base, "tape")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(tape, 0o755)
	os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644)
	common := []string{"--tape", tape, "--catalog", filepath.Join(base, "cat"), "--no-mount-check"}
	for _, args := range [][]string{
		append(append([]string{"archive", "put"}, common...), src),
		append([]string{"archive", "recover"}, common...),
	} {
		if code, _, errOut := runCmd(t, args...); code != exitFailure || !strings.Contains(errOut, "write protected") {
			t.Fatalf("%v: %d %s", args[:2], code, errOut)
		}
	}
	if ents, _ := os.ReadDir(tape); len(ents) != 0 {
		t.Fatalf("tape written: %v", ents)
	}
}

func TestCLIDriveLog(t *testing.T) {
	f := drivetest.New()
	f.ErrorLog = [][]byte{
		drivetest.DiagEntry(3, 0x11, 0, false, "E6R3", "0000000002", 0x08, 5*60*1000),
		drivetest.DiagEntry(3, 0x52, 0, true, "D8E5", "OTHERTAPE1", 0x0A, 0),
	}
	useFake(t, f, fakeFound, nil)
	code, out, _ := runCmd(t, "drive", "log")
	if code != 0 {
		t.Fatal(out)
	}
	for _, want := range []string{
		"2 of 12 slots used",
		"medium error: unrecovered read error",
		"0000000002, LTO-6 (the loaded cartridge)",
		"medium error: cartridge fault (repeated)",
		"During:        write",
		"Firmware:      D8E5 (now E6R3)",
		"When:          0h 5m after a power-on",
		"the loaded cartridge 0000000002 has 1 medium error",
		"1 entry was recorded with older firmware (D8E5)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	_, out, _ = runCmd(t, "drive", "info", "--catalog", t.TempDir())
	if !strings.Contains(out, "Error log:     2 of 12 slots used; details: tapemgr drive log") ||
		!strings.Contains(out, "error log has 1 medium error(s) for the loaded cartridge") {
		t.Fatalf("%s", out)
	}

	f.ErrorLog = nil
	if _, out, _ := runCmd(t, "drive", "log"); !strings.Contains(out, "0 of 12 slots used") || !strings.Contains(out, "No errors recorded") {
		t.Fatal(out)
	}
	f.NoErrorLog = true
	if _, out, _ := runCmd(t, "drive", "log"); !strings.Contains(out, "keeps no error log") {
		t.Fatal(out)
	}
	if _, out, _ := runCmd(t, "drive", "info", "--catalog", t.TempDir()); strings.Contains(out, "Error log:") {
		t.Fatal("error log line without a log")
	}
}
