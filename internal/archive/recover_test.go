package archive

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func recoverOpts(t *testing.T, tape string) RecoverOptions {
	return RecoverOptions{TapeRoot: tape, Catalog: newCatalog(t, tape), Log: io.Discard, ChunkSize: 16}
}

// A tape written with plain cp is adopted: files get manifest entries and
// the tape a volume record.
func TestRecoverAdoptsForeignTape(t *testing.T) {
	_, tape := setup(t)
	writeFile(t, filepath.Join(tape, "movies", "a.mkv"), "some movie data!!")
	writeFile(t, filepath.Join(tape, "b.txt"), "b")
	o := recoverOpts(t, tape)
	o.Label = "OLD-1"

	res, err := Recover(o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || res.Bytes != 18 || res.Tape.Label != "OLD-1" {
		t.Fatalf("res = %+v", res)
	}
	entries, _ := loadEntries(tape)
	for _, e := range entries {
		if !e.Recovered || e.Source != "" {
			t.Errorf("entry = %+v", e)
		}
	}
	chunks, _ := loadChunks(tape)
	if len(chunks["movies/a.mkv"].SHA256) != 2 {
		t.Errorf("chunks = %+v", chunks["movies/a.mkv"])
	}
	if v := verify(t, tape); v.Verified != 2 {
		t.Fatalf("verify = %+v", v)
	}
	if tapes, _ := o.Catalog.Tapes(); len(tapes) != 1 || tapes[0].Files != 2 {
		t.Fatalf("catalog = %+v", tapes)
	}

	// Nothing left to recover the second time.
	if res, err := Recover(o); err != nil || res.Files != 0 {
		t.Fatalf("second run: %+v, %v", res, err)
	}
}

func TestRecoverIgnoresMetadataPartialsAndSymlinks(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	writeFile(t, filepath.Join(tape, "x"+PartialSuffix), "half")
	os.Symlink("/etc/passwd", filepath.Join(tape, "link"))

	res, err := Recover(recoverOpts(t, tape))
	if err != nil || res.Files != 0 {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

// The gap left by losing both the tape manifest flush and the local pending
// log: files are on tape without records. Recover records them and a later
// put skips them instead of failing.
func TestRecoverAfterLostPendingLog(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	testHook = func(stage string, _ int64) error {
		if stage == "flush" {
			return errCrash
		}
		return nil
	}
	sum, err := Put(o)
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("err = %v", err)
	}
	os.Remove(o.Catalog.PendingPath(sum.Tape.ID))

	if _, err := Put(o); err == nil {
		t.Fatal("put should refuse to overwrite unrecorded files")
	}
	ro := recoverOpts(t, tape)
	if res, err := Recover(ro); err != nil || res.Files != 3 {
		t.Fatalf("recover = %+v, %v", res, err)
	}
	if s, err := Put(o); err != nil || s.Skipped != 3 {
		t.Fatalf("put after recover = %+v, %v", s, err)
	}
}

func TestRecoverRefusesWithPendingRecords(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	testHook = func(stage string, _ int64) error {
		if stage == "flush" {
			return errCrash
		}
		return nil
	}
	Put(o)
	testHook = nil
	if _, err := Recover(recoverOpts(t, tape)); err == nil {
		t.Fatal("recover ran with pending records")
	}
}

// A SHA256SUMS.tmp left by a crash is tapemgr's own metadata, never data.
func TestRecoverIgnoresSumsTemp(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	writeFile(t, filepath.Join(tape, "SHA256SUMS.tmp"), "half a sums file")
	res, err := Recover(recoverOpts(t, tape))
	if err != nil || res.Files != 0 {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

// An orphan with the name an encrypted file is recorded under would give
// two manifest records with one path.
func TestRecoverSkipsNameOfEncryptedFile(t *testing.T) {
	src, tape, _ := encSetup(t)
	id, _ := age.GenerateX25519Identity()
	if _, err := Put(encOpts(t, src, tape, id)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tape, "downloads", "a.iso"), "other tool")
	res, err := Recover(recoverOpts(t, tape))
	if err != nil || res.Files != 0 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	entries, _ := loadEntries(tape)
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Path] {
			t.Fatalf("two records for %s", e.Path)
		}
		seen[e.Path] = true
	}
}
