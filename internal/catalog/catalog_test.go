package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/manifest"
)

func makeTape(t *testing.T, label string, entries ...manifest.Entry) string {
	t.Helper()
	dir := t.TempDir()
	tp, err := manifest.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tp.Close()
	if _, err := tp.InitVolume(label, ""); err != nil {
		t.Fatal(err)
	}
	if err := tp.WriteSegment(manifest.Segment{Entries: entries}, entries); err != nil {
		t.Fatal(err)
	}
	return dir
}

func entry(p string, size int64, c byte) manifest.Entry {
	return manifest.Entry{Path: p, Size: size, SHA256: strings.Repeat(string(c), 64)}
}

func TestImportSearchAndVerifyHistory(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := entry("isos/copy.iso", 10, 'a')
	tape1 := makeTape(t, "B-TAPE", entry("isos/Ubuntu-24.04.iso", 10, 'a'), entry("docs/readme", 3, 'b'))
	tp, err := c.Import(tape1)
	if err != nil {
		t.Fatal(err)
	}
	ref.Ref = &manifest.Ref{Tape: tp.ID, Path: "isos/Ubuntu-24.04.iso"}
	tape2 := makeTape(t, "A-TAPE", entry("other/ubuntu-24.10.iso", 20, 'c'), ref)
	if _, err := c.Import(tape2); err != nil {
		t.Fatal(err)
	}

	if err := c.RecordVerify(tp.ID, Verification{At: time.Now(), Files: 2, Verified: 2}); err != nil {
		t.Fatal(err)
	}
	// Reimporting keeps the local verification history.
	if _, err := c.Import(tape1); err != nil {
		t.Fatal(err)
	}

	tapes, err := c.Tapes()
	if err != nil || len(tapes) != 2 {
		t.Fatalf("tapes = %+v, %v", tapes, err)
	}
	if tapes[0].Label != "A-TAPE" || tapes[1].Label != "B-TAPE" {
		t.Errorf("order = %s, %s", tapes[0].Label, tapes[1].Label)
	}
	if tapes[0].Bytes != 20 || tapes[0].Files != 2 {
		t.Errorf("reference counted as content: %+v", tapes[0])
	}
	if tapes[1].LastVerified() == nil || tapes[0].LastVerified() != nil {
		t.Errorf("verification history wrong")
	}

	hits, _ := c.Search("UBUNTU")
	if len(hits) != 2 {
		t.Errorf("search by name: %d hits", len(hits))
	}
	hits, _ = c.Search("bbbbbbbb")
	if len(hits) != 1 || hits[0].Entry.Path != "docs/readme" {
		t.Errorf("search by hash: %+v", hits)
	}

	bySize, _ := c.BySize()
	if len(bySize[10]) != 1 {
		t.Errorf("BySize includes references: %+v", bySize[10])
	}
}

func TestImportWithoutVolume(t *testing.T) {
	c, _ := Open(t.TempDir())
	if _, err := c.Import(t.TempDir()); err == nil {
		t.Fatal("import of a blank tape succeeded")
	}
}

func TestRejectsBadIDs(t *testing.T) {
	c, _ := Open(t.TempDir())
	if _, err := c.Tape("../../etc/passwd"); err == nil {
		t.Error("Tape accepted traversal ID")
	}
	if err := c.RecordVerify("../x", Verification{}); err == nil {
		t.Error("RecordVerify accepted traversal ID")
	}
	// Stray files in the catalog directory are ignored.
	os.WriteFile(filepath.Join(c.Dir, "tapes", "junk.json"), []byte("{}"), 0o644)
	if tapes, err := c.Tapes(); err != nil || len(tapes) != 0 {
		t.Errorf("tapes = %+v, %v", tapes, err)
	}
}

func TestCopiesAndRetire(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t1, err := c.Import(makeTape(t, "ONE", entry("a", 1, 'a'), entry("b", 1, 'b')))
	if err != nil {
		t.Fatal(err)
	}
	ref := entry("a-ref", 1, 'a')
	ref.Ref = &manifest.Ref{Tape: t1.ID, Path: "a"}
	t2, err := c.Import(makeTape(t, "TWO", entry("a2", 1, 'a'), ref))
	if err != nil {
		t.Fatal(err)
	}
	sumA, sumB := strings.Repeat("a", 64), strings.Repeat("b", 64)

	cp, err := c.Copies()
	if err != nil {
		t.Fatal(err)
	}
	if got := cp.Tapes(sumA, ""); len(got) != 2 {
		t.Fatalf("a: %+v", got)
	}
	if got := cp.Tapes(sumA, t1.ID); len(got) != 1 || got[0].ID != t2.ID {
		t.Fatalf("a without tape 1: %+v", got)
	}
	// The reference on tape 2 is not a copy of b or a second copy of a.
	if got := cp.Tapes(sumB, ""); len(got) != 1 || got[0].ID != t1.ID {
		t.Fatalf("b: %+v", got)
	}

	if _, err := c.SetRetired(t1.ID, true); err != nil {
		t.Fatal(err)
	}
	// Reimporting a retired tape keeps it retired.
	tr, _ := c.Tape(t1.ID)
	if tr.Retired == nil {
		t.Fatal("not retired")
	}
	cp, _ = c.Copies()
	if got := cp.Tapes(sumA, ""); len(got) != 1 || got[0].ID != t2.ID {
		t.Fatalf("a after retiring: %+v", got)
	}
	if got := cp.Tapes(sumB, ""); len(got) != 0 {
		t.Fatalf("b after retiring: %+v", got)
	}
	// Search still finds files on retired tapes.
	if hits, _ := c.Search("b"); len(hits) == 0 {
		t.Fatal("retired tape hidden from search")
	}
	if tp, err := c.SetRetired(t1.ID, false); err != nil || tp.Retired != nil {
		t.Fatalf("%+v %v", tp, err)
	}
	if _, err := c.SetRetired("00000000-0000-4000-8000-000000000000", true); err == nil {
		t.Fatal("unknown tape retired")
	}
	if _, err := c.SetRetired("../x", true); err == nil {
		t.Fatal("bad ID accepted")
	}
}

func TestRetiredSurvivesImport(t *testing.T) {
	c, _ := Open(t.TempDir())
	dir := makeTape(t, "ONE", entry("a", 1, 'a'))
	tp, _ := c.Import(dir)
	c.SetRetired(tp.ID, true)
	if _, err := c.Import(dir); err != nil {
		t.Fatal(err)
	}
	if tp, _ := c.Tape(tp.ID); tp.Retired == nil {
		t.Fatal("import cleared the retired mark")
	}
}

func TestRecordDriveEncryption(t *testing.T) {
	c, _ := Open(t.TempDir())
	dir := makeTape(t, "ENC", entry("a", 1, 'a'))
	tp, _ := c.Import(dir)
	if err := c.RecordDriveEncryption(tp.ID, "TMG000000000000000001"); err != nil {
		t.Fatal(err)
	}
	c.RecordDriveEncryption(tp.ID, "TMG000000000000000001")
	c.RecordDriveEncryption(tp.ID, "")
	c.RecordDriveEncryption(tp.ID, "TMG000000000000000002")
	// Reimporting keeps it.
	c.Import(dir)
	got, _ := c.Tape(tp.ID)
	if got.Encryption == nil || got.Encryption.Method != "drive" || len(got.Encryption.Keys) != 2 || got.Encryption.Since.IsZero() {
		t.Fatalf("%+v", got.Encryption)
	}
	if err := c.RecordDriveEncryption("00000000-0000-4000-8000-000000000000", "x"); err == nil {
		t.Fatal("unknown tape accepted")
	}
}
