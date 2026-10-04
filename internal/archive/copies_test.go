package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTape adds another empty tape next to the first one.
func newTape(t *testing.T, tape, name string) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(tape), name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNextRunSkipsFilesOnOtherTapes(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "new.bin"), "new")
	o.TapeRoot = newTape(t, tape, "tape2")
	sum, err := Put(o)
	if err != nil || sum.Files != 1 || sum.Elsewhere != 3 {
		t.Fatalf("incremental run: %+v, %v", sum, err)
	}
	o.TapeRoot = newTape(t, tape, "tape3")
	o.Again = true
	if sum, err := Put(o); err != nil || sum.Files != 4 || sum.Elsewhere != 0 {
		t.Fatalf("--again: %+v, %v", sum, err)
	}
}

func TestSecondCopy(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	o.Dedup, o.DedupMinSize = true, 1
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	tape2 := newTape(t, tape, "tape2")
	o.TapeRoot, o.Copies = tape2, 2
	sum, err := Put(o)
	// Dedup is on, but a reference to tape 1 would be no second copy.
	if err != nil || sum.Files != 3 || sum.Deduped != 0 || sum.Elsewhere != 0 {
		t.Fatalf("second copy: %+v, %v", sum, err)
	}
	entries, _ := loadEntries(tape2)
	for _, e := range entries {
		if e.Ref != nil {
			t.Fatalf("second copy holds a reference: %+v", e)
		}
	}
	// Rerunning on the same tape adds nothing: one tape is one copy.
	if sum, err := Put(o); err != nil || sum.Files != 0 || sum.Skipped != 3 {
		t.Fatalf("rerun on copy tape: %+v, %v", sum, err)
	}
	o.TapeRoot = newTape(t, tape, "tape3")
	if sum, err := Put(o); err != nil || sum.Files != 0 || sum.Elsewhere != 3 {
		t.Fatalf("third tape with two copies done: %+v, %v", sum, err)
	}
}

// A copy run on the tape that already holds the first copy cannot add a
// second one there.
func TestCopiesOnSameTapeCountOnce(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	Put(o)
	o.Copies = 2
	writeFile(t, filepath.Join(src, "new.bin"), "new")
	sum, err := Put(o)
	if err != nil || sum.Files != 1 || sum.Skipped != 3 {
		t.Fatalf("%+v, %v", sum, err)
	}
	// new.bin still has one copy, so the next tape gets all four files.
	o.TapeRoot = newTape(t, tape, "tape2")
	if sum, err := Put(o); err != nil || sum.Files != 4 {
		t.Fatalf("%+v, %v", sum, err)
	}
}

func TestDedupReferenceNeedsEnoughCopies(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	o.Dedup, o.DedupMinSize = true, 1
	Put(o)

	// The same content under another name, on another tape.
	src2 := filepath.Join(filepath.Dir(src), "more")
	writeFile(t, filepath.Join(src2, "copy.iso"), "alpha")
	o2 := o
	o2.Source, o2.TapeRoot, o2.Copies = src2, newTape(t, tape, "tape2"), 2
	if sum, err := Put(o2); err != nil || sum.Deduped != 0 || sum.Files != 1 {
		t.Fatalf("copies 2: %+v, %v", sum, err)
	}
	// alpha now has two copies, so a third tape may reference it.
	src3 := filepath.Join(filepath.Dir(src), "third")
	writeFile(t, filepath.Join(src3, "again.iso"), "alpha")
	o3 := o2
	o3.Source, o3.TapeRoot = src3, newTape(t, tape, "tape3")
	if sum, err := Put(o3); err != nil || sum.Deduped != 1 || sum.Files != 0 {
		t.Fatalf("copies satisfied: %+v, %v", sum, err)
	}
}

func TestRetiredTapeDoesNotCount(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	o.Dedup, o.DedupMinSize = true, 1
	first, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Catalog.SetRetired(first.Tape.ID, true); err != nil {
		t.Fatal(err)
	}
	tape2 := newTape(t, tape, "tape2")
	o.TapeRoot = tape2
	sum, err := Put(o)
	if err != nil || sum.Files != 3 || sum.Deduped != 0 || sum.Elsewhere != 0 {
		t.Fatalf("after retiring: %+v, %v", sum, err)
	}
	entries, _ := loadEntries(tape2)
	for _, e := range entries {
		if e.Ref != nil {
			t.Fatalf("reference to a retired tape: %+v", e)
		}
	}
}

func TestCopiesBounds(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	o.Copies = MaxCopies + 1
	if _, err := Put(o); err == nil {
		t.Fatal("too many copies accepted")
	}
	if _, err := PlanPurge(PurgeOptions{Source: src, Catalog: o.Catalog, Copies: MaxCopies + 1}); err == nil {
		t.Fatal("too many copies accepted by purge")
	}
}

// An interrupted copy run continues like any other run.
func TestInterruptedSecondCopyResumes(t *testing.T) {
	src, tape := setup(t)
	writeFile(t, filepath.Join(src, "big.bin"), strings.Repeat("b", 5000))
	o := opts(t, src, tape)
	Put(o)
	o.TapeRoot, o.Copies = newTape(t, tape, "tape2"), 2
	crashAt(t, "manifest", 5000)
	if _, err := Put(o); err == nil {
		t.Fatal("crash hook did not fire")
	}
	testHook = nil
	sum, err := Put(o)
	if err != nil || sum.Files+sum.Skipped != 4 || sum.Elsewhere != 0 {
		t.Fatalf("%+v, %v", sum, err)
	}
	if res := verify(t, o.TapeRoot); res.Verified != 4 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestPurgeNeedsVerifiedCopies(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	o.Label = "ONE"
	Put(o)
	tape2 := newTape(t, tape, "tape2")
	o.TapeRoot, o.Copies, o.Label = tape2, 2, "TWO"
	second, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(src, "a.iso")
	plan := func(copies int) *PurgePlan {
		p, err := PlanPurge(PurgeOptions{Source: one, Catalog: o.Catalog, Copies: copies})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	verifyWith(t, tape, o.Catalog)
	if p := plan(1); len(p.Delete) != 1 || p.Delete[0].Tape != "ONE" {
		t.Fatalf("one copy: %+v", p)
	}
	if p := plan(2); len(p.Delete) != 0 || !strings.Contains(p.Keep[0].Reason, "only 1 of 2 required verified copies (ONE)") {
		t.Fatalf("two copies, one verified: %+v", p)
	}
	verifyWith(t, tape2, o.Catalog)
	p := plan(2)
	if len(p.Delete) != 1 || p.Delete[0].Tape != "ONE, TWO" || len(p.Tapes) != 2 {
		t.Fatalf("two verified copies: %+v", p)
	}
	if p := plan(3); len(p.Delete) != 0 {
		t.Fatal("three copies satisfied by two tapes")
	}
	// A retired tape no longer counts.
	o.Catalog.SetRetired(second.Tape.ID, true)
	if p := plan(2); len(p.Delete) != 0 {
		t.Fatal("retired tape counted as a copy")
	}
	if p := plan(1); len(p.Delete) != 1 || p.Delete[0].Tape != "ONE" {
		t.Fatalf("%+v", p)
	}
}

func TestRestoreNamesOtherCopy(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	Put(o)
	tape2 := newTape(t, tape, "tape2")
	o.TapeRoot, o.Copies, o.Label = tape2, 2, "SPARE"
	Put(o)
	// Damage tape 1 without parity to repair it.
	os.WriteFile(filepath.Join(tape, "downloads", "a.iso"), []byte("ALPHA"), 0o644)
	var log strings.Builder
	res, err := Restore(RestoreOptions{TapeRoot: tape, Dest: t.TempDir(), Log: &log, Catalog: o.Catalog})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || !strings.Contains(log.String(), "another copy is on tape SPARE") {
		t.Fatalf("%+v\n%s", res, log.String())
	}
	// Without a catalog, restore still works and just reports the failure.
	log.Reset()
	if res, _ := Restore(RestoreOptions{TapeRoot: tape, Dest: t.TempDir(), Log: &log}); res.Failed != 1 || strings.Contains(log.String(), "another copy") {
		t.Fatalf("%+v\n%s", res, log.String())
	}
}
