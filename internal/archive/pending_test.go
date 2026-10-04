package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Knight1/tapemanager/internal/manifest"
)

func segmentCount(t *testing.T, tape string) int {
	t.Helper()
	names, _ := filepath.Glob(filepath.Join(tape, manifest.Dir, manifest.SegmentsDir, "*.manifest.jsonl"))
	return len(names)
}

// Many files in one run produce a single manifest segment, not one append
// per file.
func TestOneSegmentPerRun(t *testing.T) {
	src, tape := setup(t)
	for i := range 50 {
		writeFile(t, filepath.Join(src, "many", fmt.Sprintf("f%02d", i)), "x")
	}
	sum := put(t, src, tape)
	if sum.Files != 53 || segmentCount(t, tape) != 1 {
		t.Fatalf("files = %d, segments = %d", sum.Files, segmentCount(t, tape))
	}
	writeFile(t, filepath.Join(src, "later"), "y")
	put(t, src, tape)
	if n := segmentCount(t, tape); n != 2 {
		t.Fatalf("segments after second run = %d", n)
	}
	if res := verify(t, tape); res.Verified != 54 {
		t.Fatalf("verify = %+v", res)
	}
}

func TestFlushEvery(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	o.FlushEvery = 1
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	// a.iso flushes alone; the empty file adds no bytes and waits for b.tar.
	if n := segmentCount(t, tape); n != 2 {
		t.Fatalf("segments = %d", n)
	}
	entries, _ := loadEntries(tape)
	if len(entries) != 3 {
		t.Fatalf("entries = %d", len(entries))
	}
}

// If the tape manifest cannot be written at the end of a run, the records
// stay in the local pending log and the next run writes them first.
func TestRecoverPendingAfterFailedFlush(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	testHook = func(stage string, _ int64) error {
		if stage == "flush" {
			return errCrash
		}
		return nil
	}
	_, err := Put(o)
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("err = %v", err)
	}
	if segmentCount(t, tape) != 0 {
		t.Fatal("segment written despite failed flush")
	}

	var log strings.Builder
	Verify(VerifyOptions{TapeRoot: tape, Catalog: o.Catalog, Log: &log})
	if !strings.Contains(log.String(), "3 archived files are not yet in the tape manifest") {
		t.Errorf("verify did not warn: %s", log.String())
	}

	log.Reset()
	o.Log = &log
	sum, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "RECOVERING: writing 3 records") || sum.Skipped != 3 || sum.Files != 0 {
		t.Fatalf("summary = %+v\n%s", sum, log.String())
	}
	if res := verify(t, tape); res.Verified != 3 {
		t.Fatalf("verify = %+v", res)
	}
}

// A crash after a segment was written but before the pending log was
// cleared must not duplicate records on tape.
func TestRecoverPendingAlreadyOnTape(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	sum := put(t, src, tape)
	entries, _ := loadEntries(tape)

	var b strings.Builder
	for _, e := range entries {
		line, _ := json.Marshal(pendingRecord{Entry: e})
		b.Write(append(line, '\n'))
	}
	os.WriteFile(o.Catalog.PendingPath(sum.Tape.ID), []byte(b.String()), 0o644)

	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	after, _ := loadEntries(tape)
	if len(after) != len(entries) {
		t.Fatalf("entries grew from %d to %d", len(entries), len(after))
	}
}

func TestPendingConflictIsAnError(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	sum := put(t, src, tape)
	entries, _ := loadEntries(tape)
	e := entries[0]
	e.SHA256 = strings.Repeat("f", 64)
	line, _ := json.Marshal(pendingRecord{Entry: e})
	os.WriteFile(o.Catalog.PendingPath(sum.Tape.ID), append(line, '\n'), 0o644)
	if _, err := Put(o); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("err = %v", err)
	}
}

func TestPendingLogTornTail(t *testing.T) {
	name := filepath.Join(t.TempDir(), "p.jsonl")
	rec := pendingRecord{Entry: manifest.Entry{Path: "a", Size: 3, SHA256: strings.Repeat("a", 64)}}
	line, _ := json.Marshal(rec)
	os.WriteFile(name, append(append(line, '\n'), []byte(`{"entry":{"pa`)...), 0o644)

	p, err := openPending(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.records) != 1 || p.bytes != 3 {
		t.Fatalf("records = %+v", p.records)
	}
	// New records start on a clean line after the torn tail is cut.
	rec.Entry.Path = "b"
	if err := p.add(rec); err != nil {
		t.Fatal(err)
	}
	p.close()
	p, err = openPending(name)
	if err != nil || len(p.records) != 2 {
		t.Fatalf("reopen: %+v, %v", p, err)
	}
	p.close()
}

func TestPendingLogCorruptMiddle(t *testing.T) {
	name := filepath.Join(t.TempDir(), "p.jsonl")
	good, _ := json.Marshal(pendingRecord{Entry: manifest.Entry{Path: "a", Size: 1, SHA256: strings.Repeat("a", 64)}})
	data := string(good) + "\nbroken\n" + string(good) + "\n"
	os.WriteFile(name, []byte(data), 0o644)
	if _, err := openPending(name); err == nil {
		t.Fatal("corrupt line in the middle accepted")
	}
}

func TestPendingLogRejectsUntrusted(t *testing.T) {
	name := filepath.Join(t.TempDir(), "p.jsonl")
	sha := strings.Repeat("a", 64)
	for _, line := range []string{
		`{"entry":{"path":"../x","size":1,"sha256":"` + sha + `"}}`,
		`{"entry":{"path":"a","size":1,"sha256":"` + sha + `"},"chunks":{"path":"b","chunk_size":16,"sha256":[]}}`,
		`{"entry":{"path":"a","size":1,"sha256":"` + sha + `"},"chunks":{"path":"a","chunk_size":1,"sha256":[]}}`,
	} {
		// Followed by a good line so the bad one is not treated as a torn tail.
		good := `{"entry":{"path":"z","size":1,"sha256":"` + sha + `"}}`
		os.WriteFile(name, []byte(line+"\n"+good+"\n"), 0o644)
		if _, err := openPending(name); err == nil {
			t.Errorf("accepted %s", line)
		}
	}
}

func TestPendingCountMissing(t *testing.T) {
	if n, err := pendingCount(filepath.Join(t.TempDir(), "none")); n != 0 || err != nil {
		t.Fatalf("%d, %v", n, err)
	}
}
