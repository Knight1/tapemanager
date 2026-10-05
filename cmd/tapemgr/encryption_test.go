package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/drive/drivetest"
)

// useEncryption makes the drive behind every tape report st, or err.
func useEncryption(t *testing.T, st *drive.EncryptionStatus, err error) {
	t.Helper()
	old := driveEncryption
	t.Cleanup(func() { driveEncryption = old })
	driveEncryption = func(string) (*drive.EncryptionStatus, error) { return st, err }
}

var (
	encOn  = &drive.EncryptionStatus{EncryptMode: 2, DecryptMode: 3, KeyID: []byte{'T', 'M', 'G', 0, 0, 0, 0, 0, 0, 0, 0, 1}}
	encOn2 = &drive.EncryptionStatus{EncryptMode: 2, DecryptMode: 3, KeyID: []byte{'T', 'M', 'G', 0, 0, 0, 0, 0, 0, 0, 0, 2}}
	encOff = &drive.EncryptionStatus{}
)

type encEnv struct {
	src, tape, cat string
}

func newEncEnv(t *testing.T) encEnv {
	base := t.TempDir()
	e := encEnv{filepath.Join(base, "src"), filepath.Join(base, "tape"), filepath.Join(base, "cat")}
	os.MkdirAll(e.src, 0o755)
	os.MkdirAll(e.tape, 0o755)
	os.WriteFile(filepath.Join(e.src, "a"), []byte("alpha"), 0o644)
	return e
}

func (e encEnv) put(t *testing.T, extra ...string) (int, string, string) {
	t.Helper()
	args := append([]string{"archive", "put", "--tape", e.tape, "--catalog", e.cat, "--no-mount-check"}, extra...)
	return runCmd(t, append(args, e.src)...)
}

func (e encEnv) tapeRecord(t *testing.T) *catalog.Tape {
	t.Helper()
	c, _ := catalog.Open(e.cat)
	tapes, err := c.Tapes()
	if err != nil || len(tapes) != 1 {
		t.Fatalf("%v %v", tapes, err)
	}
	return &tapes[0]
}

func TestRequireEncryption(t *testing.T) {
	e := newEncEnv(t)
	useEncryption(t, nil, errors.New("not a mount point"))
	if code, _, errOut := e.put(t, "--require-encryption"); code != exitFailure || !strings.Contains(errOut, "cannot read the drive's encryption setting") {
		t.Fatalf("unknown: %d %s", code, errOut)
	}
	useEncryption(t, encOff, nil)
	if code, _, errOut := e.put(t, "--require-encryption"); code != exitFailure || !strings.Contains(errOut, "not encrypting") {
		t.Fatalf("off: %d %s", code, errOut)
	}
	if ents, _ := os.ReadDir(e.tape); len(ents) != 0 {
		t.Fatalf("tape written: %v", ents)
	}
	t.Setenv("TAPEMGR_REQUIRE_ENCRYPTION", "yes")
	if code, _, _ := e.put(t); code != exitFailure {
		t.Fatal("environment default ignored")
	}

	useEncryption(t, encOn, nil)
	if code, _, errOut := e.put(t); code != 0 {
		t.Fatalf("on: %d %s", code, errOut)
	}
	rec := e.tapeRecord(t)
	if rec.Encryption == nil || len(rec.Encryption.Keys) != 1 || rec.Encryption.Keys[0] != "TMG000000000000000001" {
		t.Fatalf("%+v", rec.Encryption)
	}
	if code, out, _ := runCmd(t, "catalog", "tapes", "--catalog", e.cat); code != 0 || !strings.Contains(out, "encrypted (key TMG000000000000000001)") {
		t.Fatalf("tapes: %s", out)
	}
}

func TestEncryptedTapeNeverGetsPlainData(t *testing.T) {
	e := newEncEnv(t)
	useEncryption(t, encOn, nil)
	if code, _, errOut := e.put(t); code != 0 {
		t.Fatal(errOut)
	}
	os.WriteFile(filepath.Join(e.src, "b"), []byte("bravo"), 0o644)

	// Mounted without the key: refused even without --require-encryption.
	useEncryption(t, encOff, nil)
	if code, _, errOut := e.put(t); code != exitFailure || !strings.Contains(errOut, "holds encrypted data, but the drive is not encrypting") {
		t.Fatalf("off: %d %s", code, errOut)
	}
	useEncryption(t, nil, errors.New("no drive"))
	if code, _, errOut := e.put(t); code != exitFailure || !strings.Contains(errOut, "cannot be read") {
		t.Fatalf("unknown: %d %s", code, errOut)
	}
	recover := []string{"archive", "recover", "--tape", e.tape, "--catalog", e.cat, "--no-mount-check"}
	if code, _, errOut := runCmd(t, recover...); code != exitFailure || !strings.Contains(errOut, "holds encrypted data") {
		t.Fatalf("recover: %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(e.tape, "src", "b")); err == nil {
		t.Fatal("plain data written to an encrypted tape")
	}

	// Another key works, with a note, and both keys are recorded.
	useEncryption(t, encOn2, nil)
	code, out, errOut := e.put(t)
	if code != 0 || !strings.Contains(out, "both keys are needed") {
		t.Fatalf("second key: %d %s %s", code, out, errOut)
	}
	if rec := e.tapeRecord(t); len(rec.Encryption.Keys) != 2 {
		t.Fatalf("%+v", rec.Encryption)
	}
}

func TestPlainTapeStaysPlainWithoutDrive(t *testing.T) {
	e := newEncEnv(t)
	useEncryption(t, nil, errors.New("no drive"))
	if code, _, errOut := e.put(t); code != 0 {
		t.Fatal(errOut)
	}
	if rec := e.tapeRecord(t); rec.Encryption != nil {
		t.Fatalf("%+v", rec.Encryption)
	}
}

var keyFilePattern = regexp.MustCompile(`^(DK=[A-Za-z0-9+/]{43}=\nDKi=TMG[0-9a-f]{18}\n)+$`)

func TestDriveKeygen(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "sub", "keys")
	code, out, errOut := runCmd(t, "drive", "keygen", "--out", keys)
	if code != 0 || !strings.Contains(out, "kmi_dki_for_format=TMG") || !strings.Contains(out, "Back up") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	b, _ := os.ReadFile(keys)
	if !keyFilePattern.Match(b) {
		t.Fatalf("key file %q", b)
	}
	// The key itself is never printed.
	if dk := strings.TrimPrefix(strings.Split(string(b), "\n")[0], "DK="); strings.Contains(out, dk) {
		t.Fatal("key printed")
	}
	if st, _ := os.Stat(keys); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	// A second key is added; the first is kept.
	if code, _, _ := runCmd(t, "drive", "keygen", "--out", keys, "--prefix", "AB1"); code != 0 {
		t.Fatal("second key")
	}
	b2, _ := os.ReadFile(keys)
	if !strings.HasPrefix(string(b2), string(b)) || strings.Count(string(b2), "DKi=") != 2 || !strings.Contains(string(b2), "DKi=AB1") {
		t.Fatalf("%q", b2)
	}

	os.Chmod(keys, 0o640)
	if code, _, errOut := runCmd(t, "drive", "keygen", "--out", keys); code != exitFailure || !strings.Contains(errOut, "chmod 600") {
		t.Fatalf("readable by group: %d %s", code, errOut)
	}
	link := filepath.Join(dir, "link")
	os.Symlink(keys, link)
	if code, _, errOut := runCmd(t, "drive", "keygen", "--out", link); code != exitFailure || !strings.Contains(errOut, "not a regular file") {
		t.Fatalf("symlink: %d %s", code, errOut)
	}
	for _, bad := range []string{"AB", "ABCD", "A/B", "a:b"} {
		if code, _, _ := runCmd(t, "drive", "keygen", "--out", keys, "--prefix", bad); code != exitUsage {
			t.Fatalf("prefix %q accepted", bad)
		}
	}
	if code, _, _ := runCmd(t, "drive", "keygen"); code != exitUsage {
		t.Fatal("missing --out accepted")
	}
}

func TestDriveInfoShowsEncryption(t *testing.T) {
	f := drivetest.New()
	f.Encrypting = true
	f.KeyID = []byte{'T', 'M', 'G', 0, 0, 0, 0, 0, 0, 0, 0, 7}
	useFake(t, f, fakeFound, nil)
	if code, out, _ := runCmd(t, "drive", "info", "--catalog", t.TempDir()); code != 0 || !strings.Contains(out, "Encryption:    on, AES-256-GCM, key TMG000000000000000007") {
		t.Fatalf("%d %s", code, out)
	}
	f.Encrypting = false
	if _, out, _ := runCmd(t, "drive", "info", "--catalog", t.TempDir()); !strings.Contains(out, "Encryption:    off (supports AES-256-GCM for the loaded cartridge)") {
		t.Fatalf("%s", out)
	}
}

func TestDriveEncryptionDefault(t *testing.T) {
	f := drivetest.New()
	f.Encrypting = true
	f.KeyID = []byte("k")
	useFake(t, f, fakeFound, nil)
	oldFor := forMount
	t.Cleanup(func() { forMount = oldFor })
	forMount = func(string) (drive.Found, error) { return fakeFound, nil }
	// TestMain replaced driveEncryption; check the real one here.
	st, err := realDriveEncryption("/mnt/ltfs")
	if err != nil || !st.Encrypting() || st.KeyName() != "k" {
		t.Fatalf("%+v %v", st, err)
	}
	forMount = func(string) (drive.Found, error) { return drive.Found{}, errors.New("not a mount point") }
	if _, err := realDriveEncryption("/mnt/ltfs"); err == nil {
		t.Fatal("missing drive not reported")
	}
}
