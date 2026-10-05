package archive

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Knight1/tapemanager/internal/manifest"
)

var errCrash = errors.New("simulated crash")

// crashAt makes Put fail at stage once offset reaches at.
func crashAt(t *testing.T, stage string, at int64) {
	t.Helper()
	testHook = func(s string, off int64) error {
		if s == stage && off >= at {
			return errCrash
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
}

// bigSetup creates one 1000 byte file. With 16 byte chunks and checkpoints
// every 64 bytes it gives plenty of resume points.
func bigSetup(t *testing.T) (src, tape string, data []byte, o PutOptions) {
	t.Helper()
	base := t.TempDir()
	src = filepath.Join(base, "src")
	tape = filepath.Join(base, "tape")
	data = make([]byte, 1000)
	rand.Read(data)
	writeFile(t, filepath.Join(src, "big.bin"), string(data))
	os.MkdirAll(tape, 0o755)
	o = opts(t, src, tape)
	o.ChunkSize = 16
	o.CheckpointEvery = 64
	return
}

func checkTapeFile(t *testing.T, tape string, data []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(tape, "src", "big.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("tape content differs (%d bytes, %v)", len(got), err)
	}
	if res := verify(t, tape); res.Verified != 1 {
		t.Fatalf("verify = %+v", res)
	}
}

func TestResumeMidFile(t *testing.T) {
	_, tape, data, o := bigSetup(t)
	crashAt(t, "checkpoint", 512)
	if _, err := Put(o); !errors.Is(err, errCrash) {
		t.Fatalf("err = %v", err)
	}
	testHook = nil

	sum, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 512 || sum.Files != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	checkTapeFile(t, tape, data)
	if matches, _ := filepath.Glob(filepath.Join(o.Catalog.Dir, "journal", "*", "*")); len(matches) != 0 {
		t.Errorf("journal left behind: %v", matches)
	}
}

// LTFS can lose data written after its last index update. The partial file
// is then shorter than the newest checkpoint and an older one must be used.
func TestResumeAfterLostTail(t *testing.T) {
	_, tape, data, o := bigSetup(t)
	crashAt(t, "checkpoint", 512)
	Put(o)
	testHook = nil

	partial := filepath.Join(tape, "src", "big.bin"+PartialSuffix)
	if err := os.Truncate(partial, 300); err != nil {
		t.Fatal(err)
	}
	sum, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 256 {
		t.Fatalf("resumed at %d, want 256", sum.Resumed)
	}
	checkTapeFile(t, tape, data)
}

func TestResumeRejectsCorruptPartial(t *testing.T) {
	_, tape, data, o := bigSetup(t)
	crashAt(t, "checkpoint", 512)
	Put(o)
	testHook = nil

	partial := filepath.Join(tape, "src", "big.bin"+PartialSuffix)
	damage(t, partial, 500, 4)

	sum, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 448 {
		t.Fatalf("resumed at %d, want 448 (checkpoint before the damage)", sum.Resumed)
	}
	checkTapeFile(t, tape, data)
}

func TestResumeRestartsWhenSourceChanged(t *testing.T) {
	src, tape, _, o := bigSetup(t)
	crashAt(t, "checkpoint", 512)
	Put(o)
	testHook = nil

	data := bytes.Repeat([]byte("z"), 900)
	writeFile(t, filepath.Join(src, "big.bin"), string(data))
	sum, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 0 {
		t.Fatalf("resumed = %d", sum.Resumed)
	}
	checkTapeFile(t, tape, data)
}

func TestResumeAfterRenameBeforeManifest(t *testing.T) {
	_, tape, data, o := bigSetup(t)
	crashAt(t, "manifest", 0)
	if _, err := Put(o); !errors.Is(err, errCrash) {
		t.Fatalf("err = %v", err)
	}
	testHook = nil

	sum, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 1 || sum.Resumed != 1000 {
		t.Fatalf("summary = %+v", sum)
	}
	checkTapeFile(t, tape, data)
}

func TestVerifyReportsDamagedRange(t *testing.T) {
	_, tape, _, o := bigSetup(t)
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	damage(t, filepath.Join(tape, "src", "big.bin"), 40, 1)

	var log strings.Builder
	res, err := Verify(VerifyOptions{TapeRoot: tape, Log: &log})
	if err != nil || res.Failed != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if !strings.Contains(log.String(), "damaged bytes: 32-47") {
		t.Errorf("log = %s", log.String())
	}
}

func TestChunksRecorded(t *testing.T) {
	_, tape, _, o := bigSetup(t)
	Put(o)
	chunks, err := loadChunks(tape)
	if err != nil {
		t.Fatal(err)
	}
	c := chunks["src/big.bin"]
	if c.ChunkSize != 16 || len(c.SHA256) != 63 {
		t.Fatalf("chunks = %d of %d bytes", len(c.SHA256), c.ChunkSize)
	}
}

func TestDedup(t *testing.T) {
	src, tape := setup(t)
	writeFile(t, filepath.Join(src, "copy.iso"), "alpha")
	o := opts(t, src, tape)
	o.Dedup = true
	o.DedupMinSize = 1
	sum, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	// a.iso is written, copy.iso is identical and becomes a reference.
	if sum.Files != 3 || sum.Deduped != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if _, err := os.Stat(filepath.Join(tape, "downloads", "copy.iso")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("duplicate content was written")
	}
	entries, _ := loadEntries(tape)
	var ref *manifest.Ref
	for _, e := range entries {
		if e.Path == "downloads/copy.iso" {
			ref = e.Ref
		}
	}
	if ref == nil || ref.Path != "downloads/a.iso" || ref.Tape != sum.Tape.ID {
		t.Fatalf("ref = %+v", ref)
	}
	sums, _ := os.ReadFile(filepath.Join(tape, manifest.SumsName))
	if strings.Contains(string(sums), "copy.iso") {
		t.Errorf("reference listed in SHA256SUMS")
	}
	res := verify(t, tape)
	if res.Refs != 1 || res.Verified != 3 || res.Failed != 0 {
		t.Fatalf("verify = %+v", res)
	}

	// A second tape finds the copy through the catalog.
	tape2 := filepath.Join(filepath.Dir(tape), "tape2")
	os.MkdirAll(tape2, 0o755)
	o.TapeRoot = tape2
	o.Again = true // archive again although already on tape 1
	sum, err = Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Deduped != 3 || sum.Files != 1 { // only the empty file is below the size floor
		t.Fatalf("second tape summary = %+v", sum)
	}
}

func TestCatalogUpdated(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	o.Label = "NAS-2026-001"
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(VerifyOptions{TapeRoot: tape, Catalog: o.Catalog, Log: io.Discard}); err != nil {
		t.Fatal(err)
	}
	tapes, err := o.Catalog.Tapes()
	if err != nil || len(tapes) != 1 {
		t.Fatalf("tapes = %+v, %v", tapes, err)
	}
	tp := tapes[0]
	if tp.Label != "NAS-2026-001" || tp.Files != 3 || tp.Bytes != 10 || tp.LastVerified() == nil {
		t.Fatalf("tape = %+v", tp)
	}
	hits, _ := o.Catalog.Search("SUB/B")
	if len(hits) != 1 || hits[0].Entry.Path != "downloads/sub/b.tar" {
		t.Fatalf("hits = %+v", hits)
	}
}

// A partial file that cannot be cut back to the resume point must stop the
// run; writing on would put the rest at the start of the file.
func TestResumeTruncateFails(t *testing.T) {
	_, tape, data, o := bigSetup(t)
	crashAt(t, "checkpoint", 512)
	if _, err := Put(o); !errors.Is(err, errCrash) {
		t.Fatalf("err = %v", err)
	}
	testHook = nil
	errTrunc := errors.New("truncate failed")
	truncateFile = func(*os.File, int64) error { return errTrunc }
	_, err := Put(o)
	truncateFile = (*os.File).Truncate
	if !errors.Is(err, errTrunc) {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := loadEntries(tape); len(entries) != 0 {
		t.Fatalf("recorded %d files after a failed resume", len(entries))
	}
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	checkTapeFile(t, tape, data)
}

// A crash between recording a file and deleting its journal leaves the
// journal; the next run removes it, and with it the staged parity.
func TestRecordedJournalRemoved(t *testing.T) {
	_, tape, data, o := bigSetup(t)
	o.Parity = 10
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	vol := loadVolumeID(t, tape)
	jpath := journalPath(o.Catalog.JournalDir(vol), "src/big.bin")
	ppath := filepath.Join(o.Catalog.ParityDir(vol), fileKey("src/big.bin")+".bin")
	writeFile(t, jpath, "{}\n")
	writeFile(t, ppath, "stale parity")
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{jpath, ppath} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind: %v", p, err)
		}
	}
	checkTapeFile(t, tape, data)
}

func loadVolumeID(t *testing.T, tape string) string {
	t.Helper()
	tp, err := manifest.Open(tape)
	if err != nil {
		t.Fatal(err)
	}
	defer tp.Close()
	v, err := tp.Volume()
	if err != nil || v == nil {
		t.Fatalf("%v %v", v, err)
	}
	return v.ID
}
