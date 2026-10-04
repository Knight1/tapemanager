package archive

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/manifest"
)

func planPurge(t *testing.T, src, tape string, c *catalog.Catalog, rehash bool) *PurgePlan {
	t.Helper()
	p, err := PlanPurge(PurgeOptions{Source: src, TapeRoot: tape, Catalog: c, Rehash: rehash})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func verifyWith(t *testing.T, tape string, c *catalog.Catalog) {
	t.Helper()
	if _, err := Verify(VerifyOptions{TapeRoot: tape, Catalog: c, Log: io.Discard}); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestPurgeRequiresVerification(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)

	p := planPurge(t, src, tape, o.Catalog, false)
	if len(p.Delete) != 0 || len(p.Keep) != 3 || !strings.Contains(p.Keep[0].Reason, "not passed verification") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPurgeAfterVerification(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	o.Label = "NAS-1"
	Put(o)
	verifyWith(t, tape, o.Catalog)

	p := planPurge(t, src, tape, o.Catalog, false)
	if len(p.Delete) != 3 || len(p.Keep) != 0 || p.Bytes != 10 || len(p.Tapes) != 1 || p.Tapes[0] != "NAS-1" {
		t.Fatalf("plan = %+v", p)
	}
	n, err := p.Execute(io.Discard)
	if err != nil || n != 3 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if exists(filepath.Join(src, "a.iso")) || exists(filepath.Join(src, "sub")) {
		t.Error("files or empty directories left behind")
	}
	if !exists(src) {
		t.Error("source root itself was removed")
	}
}

func TestPurgeKeepsUnarchivedAndChanged(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)
	verifyWith(t, tape, o.Catalog)

	writeFile(t, filepath.Join(src, "sub", "new.txt"), "new")
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(src, "a.iso"), later, later)

	p := planPurge(t, src, tape, o.Catalog, false)
	reasons := map[string]string{}
	for _, k := range p.Keep {
		reasons[filepath.Base(k.Path)] = k.Reason
	}
	if len(p.Delete) != 2 || reasons["new.txt"] != "not archived" || !strings.Contains(reasons["a.iso"], "changed since archiving") {
		t.Fatalf("delete = %d, keep = %v", len(p.Delete), reasons)
	}
	p.Execute(io.Discard)
	// sub still holds new.txt and must survive the empty directory cleanup.
	if !exists(filepath.Join(src, "sub", "new.txt")) || !exists(filepath.Join(src, "a.iso")) {
		t.Fatal("kept file was deleted")
	}
}

func TestPurgeFailedVerificationRevokes(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)
	verifyWith(t, tape, o.Catalog)
	writeFile(t, filepath.Join(tape, "downloads", "a.iso"), "alphX")
	Verify(VerifyOptions{TapeRoot: tape, Catalog: o.Catalog, Log: io.Discard})

	if p := planPurge(t, src, tape, o.Catalog, false); len(p.Delete) != 0 {
		t.Fatalf("purge allowed after a failed verification: %+v", p.Delete)
	}
}

func TestPurgeDeduplicatedNeedsContentTapeVerified(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	o.Dedup, o.DedupMinSize = true, 1
	Put(o)

	// Second tape holds only references to tape 1.
	tape2 := filepath.Join(filepath.Dir(tape), "tape2")
	os.MkdirAll(tape2, 0o755)
	o2 := o
	o2.TapeRoot = tape2
	o2.Source = filepath.Join(src, "a.iso")
	o2.Prefix = "again/a.iso"
	if sum, err := Put(o2); err != nil || sum.Deduped != 1 {
		t.Fatalf("%+v, %v", sum, err)
	}
	verifyWith(t, tape2, o.Catalog)

	one := filepath.Join(src, "a.iso")
	if p := planPurge(t, one, "", o.Catalog, false); len(p.Delete) != 0 {
		t.Fatal("purged through an unverified content tape")
	}
	verifyWith(t, tape, o.Catalog)
	if p := planPurge(t, one, "", o.Catalog, false); len(p.Delete) != 1 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPurgeRehash(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)
	verifyWith(t, tape, o.Catalog)

	// Same size and mtime, different content.
	f := filepath.Join(src, "a.iso")
	st, _ := os.Stat(f)
	os.WriteFile(f, []byte("ALPHA"), 0o644)
	os.Chtimes(f, st.ModTime(), st.ModTime())

	if p := planPurge(t, f, tape, o.Catalog, false); len(p.Delete) != 1 {
		t.Fatal("size and mtime check should not notice")
	}
	p := planPurge(t, f, tape, o.Catalog, true)
	if len(p.Delete) != 0 || p.Keep[0].Reason != "content differs from the archived copy" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPurgeRecheckBeforeDelete(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)
	verifyWith(t, tape, o.Catalog)

	p := planPurge(t, src, tape, o.Catalog, false)
	writeFile(t, filepath.Join(src, "a.iso"), "replaced after planning")
	n, _ := p.Execute(io.Discard)
	if n != 2 || !exists(filepath.Join(src, "a.iso")) {
		t.Fatalf("deleted %d; changed file deleted = %v", n, !exists(filepath.Join(src, "a.iso")))
	}
}

// A directory swapped for a symlink between planning and deletion must not
// redirect the deletion outside the source.
func TestPurgeSymlinkSwap(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)
	verifyWith(t, tape, o.Catalog)
	p := planPurge(t, src, tape, o.Catalog, false)

	outside := filepath.Join(filepath.Dir(src), "outside")
	writeFile(t, filepath.Join(outside, "b.tar"), "bravo")
	os.RemoveAll(filepath.Join(src, "sub"))
	os.Symlink(outside, filepath.Join(src, "sub"))

	p.Execute(io.Discard)
	if !exists(filepath.Join(outside, "b.tar")) {
		t.Fatal("deleted a file outside the source through a symlink")
	}
}

func TestPurgeIgnoresSymlinksInSource(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)
	verifyWith(t, tape, o.Catalog)
	os.Symlink(filepath.Join(src, "a.iso"), filepath.Join(src, "link"))

	p := planPurge(t, src, tape, o.Catalog, false)
	p.Execute(io.Discard)
	if !exists(filepath.Join(src, "link")) {
		t.Fatal("symlink was deleted")
	}
}

func TestPurgeRefusesTapeOverlap(t *testing.T) {
	src, tape := setup(t)
	c := newCatalog(t, tape)
	for _, s := range []string{tape, filepath.Join(tape, "x"), filepath.Dir(tape)} {
		os.MkdirAll(s, 0o755)
		if _, err := PlanPurge(PurgeOptions{Source: s, TapeRoot: tape, Catalog: c}); err == nil || !strings.Contains(err.Error(), "overlaps") {
			t.Errorf("%s: err = %v", s, err)
		}
	}
	if _, err := PlanPurge(PurgeOptions{Source: src, TapeRoot: tape, Catalog: c}); err != nil {
		t.Errorf("sibling directory refused: %v", err)
	}
}

func TestPurgeMissingSource(t *testing.T) {
	_, tape := setup(t)
	_, err := PlanPurge(PurgeOptions{Source: filepath.Join(tape, "..", "nope"), Catalog: newCatalog(t, tape)})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}

// A tape from elsewhere can claim any source path. Its records must never
// cause local files to be deleted, even after the tape passes verification.
func TestPurgeIgnoresForeignTapeClaims(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "nas", "important.iso")
	writeFile(t, victim, "precious")
	st, _ := os.Stat(victim)

	// Forge a tape that claims to hold the victim, with matching size and
	// mtime but different content.
	foreign := filepath.Join(base, "foreign")
	writeFile(t, filepath.Join(foreign, "important.iso"), "xxxxxxxx")
	os.Chtimes(filepath.Join(foreign, "important.iso"), st.ModTime(), st.ModTime())
	c := newCatalog(t, foreign)
	if _, err := Recover(RecoverOptions{TapeRoot: foreign, Catalog: c, Log: io.Discard}); err != nil {
		t.Fatal(err)
	}
	tp, _ := loadEntries(foreign)
	forged := tp[0]
	forged.Source = victim
	forged.Recovered = false
	os.RemoveAll(filepath.Join(foreign, ".tapemgr", "segments"))
	tape, _ := manifest.Open(foreign)
	tape.WriteSegment(manifest.Segment{Entries: []manifest.Entry{forged}}, []manifest.Entry{forged})
	tape.Close()
	c.Import(foreign)
	verifyWith(t, foreign, c)

	p := planPurge(t, filepath.Dir(victim), "", c, false)
	if len(p.Delete) != 0 {
		t.Fatalf("foreign tape claim accepted: %+v", p.Delete)
	}
}

// Losing the pending log clear after a flush must not lose the record of
// what this machine wrote.
func TestWrittenRecordSurvivesFlushCrash(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	sum := put(t, src, tape)
	// Simulate the crash: written log lost, pending log still full.
	os.RemoveAll(filepath.Join(o.Catalog.Dir, "written"))
	entries, _ := loadEntries(tape)
	p, _ := openPending(o.Catalog.PendingPath(sum.Tape.ID))
	for _, e := range entries {
		p.add(pendingRecord{Entry: e})
	}
	p.close()

	put(t, src, tape)
	verifyWith(t, tape, o.Catalog)
	if plan := planPurge(t, src, tape, o.Catalog, false); len(plan.Delete) != 3 {
		t.Fatalf("plan = %+v", plan)
	}
}
