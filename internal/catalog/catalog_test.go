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
	if _, err := manifest.InitVolume(dir, label, ""); err != nil {
		t.Fatal(err)
	}
	w, err := manifest.OpenWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := w.Append(e, nil); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
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
