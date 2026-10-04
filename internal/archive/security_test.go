package archive

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Knight1/tapemanager/internal/manifest"
)

// A tape directory that is a symlink must not redirect writes outside the
// tape.
func TestPutRefusesSymlinkOutOfTape(t *testing.T) {
	src, tape := setup(t)
	outside := filepath.Join(filepath.Dir(tape), "outside")
	os.MkdirAll(outside, 0o755)
	if err := os.Symlink(outside, filepath.Join(tape, "downloads")); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(opts(t, src, tape)); err == nil {
		t.Fatal("put followed a symlink out of the tape")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("files written outside the tape: %v", entries)
	}
}

// A manifest entry whose file is a symlink to somewhere else must not be
// read through.
func TestVerifyRefusesSymlinkOutOfTape(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	secret := filepath.Join(filepath.Dir(tape), "secret")
	writeFile(t, secret, "alpha")
	p := filepath.Join(tape, "downloads", "a.iso")
	os.Remove(p)
	os.Symlink(secret, p)

	res, err := Verify(VerifyOptions{TapeRoot: tape, Log: io.Discard})
	if err != nil || res.Failed != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

func TestVerifyRejectsTraversalInManifest(t *testing.T) {
	_, tape := setup(t)
	os.MkdirAll(filepath.Join(tape, manifest.Dir, manifest.SegmentsDir), 0o755)
	line := `{"path":"../../../../etc/passwd","size":1,"sha256":"` + strings.Repeat("0", 64) + `"}` + "\n"
	os.WriteFile(filepath.Join(tape, manifest.Dir, manifest.SegmentsDir, "000001.manifest.jsonl"), []byte(line), 0o644)
	// The bad entry is skipped and reported, never followed.
	res, err := Verify(VerifyOptions{TapeRoot: tape, Log: io.Discard})
	if err != nil || res.Problems != 1 || res.Files != 0 || res.Verified != 0 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestPutRejectsCraftedVolumeID(t *testing.T) {
	src, tape := setup(t)
	os.MkdirAll(filepath.Join(tape, manifest.Dir), 0o755)
	os.WriteFile(filepath.Join(tape, manifest.Dir, manifest.VolumeName), []byte(`{"id":"../../evil"}`), 0o644)
	o := opts(t, src, tape)
	if _, err := Put(o); err == nil {
		t.Fatal("crafted volume ID accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(o.Catalog.Dir), "evil.json")); err == nil {
		t.Fatal("catalog wrote outside its directory")
	}
}

// Damaged journals must never crash the resume logic.
func TestResumeIgnoresBogusJournalOffsets(t *testing.T) {
	_, tape, data, o := bigSetup(t)
	crashAt(t, "checkpoint", 512)
	Put(o)
	testHook = nil

	jdir, _ := filepath.Glob(filepath.Join(o.Catalog.Dir, "journal", "*"))
	jfiles, _ := filepath.Glob(filepath.Join(jdir[0], "*.jsonl"))
	b, _ := os.ReadFile(jfiles[0])
	hdr := strings.SplitN(string(b), "\n", 2)[0]
	bogus := hdr + "\n" +
		`{"offset":-5,"state":"","chunks":[]}` + "\n" +
		`{"offset":9223372036854775807,"state":"","chunks":["x"]}` + "\n" +
		`{"offset":64,"state":"","chunks":[]}` + "\n"
	os.WriteFile(jfiles[0], []byte(bogus), 0o644)

	sum, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 0 {
		t.Fatalf("resumed from bogus checkpoint at %d", sum.Resumed)
	}
	checkTapeFile(t, tape, data)
}

func TestPutRejectsReservedDest(t *testing.T) {
	src, tape := setup(t)
	for _, p := range []string{"SHA256SUMS", ".tapemgr"} {
		o := opts(t, filepath.Join(src, "a.iso"), tape)
		o.Prefix = p
		if _, err := Put(o); err == nil {
			t.Errorf("dest %q accepted", p)
		}
	}
}
