package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/manifest"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func setup(t *testing.T) (src, tape string) {
	t.Helper()
	base := t.TempDir()
	src = filepath.Join(base, "downloads")
	tape = filepath.Join(base, "tape")
	writeFile(t, filepath.Join(src, "a.iso"), "alpha")
	writeFile(t, filepath.Join(src, "sub", "b.tar"), "bravo")
	writeFile(t, filepath.Join(src, "empty"), "")
	if err := os.MkdirAll(tape, 0o755); err != nil {
		t.Fatal(err)
	}
	return src, tape
}

func loadEntries(dir string) ([]manifest.Entry, error) {
	tp, err := manifest.Open(dir)
	if err != nil {
		return nil, err
	}
	defer tp.Close()
	return tp.Entries()
}

func loadChunks(dir string) (map[string]manifest.Chunks, error) {
	tp, err := manifest.Open(dir)
	if err != nil {
		return nil, err
	}
	defer tp.Close()
	return tp.Chunks()
}

func newCatalog(t *testing.T, tape string) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Open(filepath.Join(filepath.Dir(tape), "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func opts(t *testing.T, src, tape string) PutOptions {
	return PutOptions{TapeRoot: tape, Source: src, Catalog: newCatalog(t, tape), Log: io.Discard}
}

func put(t *testing.T, src, tape string) PutSummary {
	t.Helper()
	sum, err := Put(opts(t, src, tape))
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func verify(t *testing.T, tape string) VerifyResult {
	t.Helper()
	res, err := Verify(VerifyOptions{TapeRoot: tape, Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestPutWritesFilesAndManifest(t *testing.T) {
	src, tape := setup(t)
	sum := put(t, src, tape)
	if sum.Files != 3 || sum.Bytes != 10 {
		t.Fatalf("summary = %+v", sum)
	}

	got, err := os.ReadFile(filepath.Join(tape, "downloads", "sub", "b.tar"))
	if err != nil || string(got) != "bravo" {
		t.Fatalf("tape content = %q, %v", got, err)
	}

	entries, err := loadEntries(tape)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"downloads/a.iso", "downloads/empty", "downloads/sub/b.tar"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v", entries)
	}
	for i, e := range entries {
		if e.Path != want[i] {
			t.Errorf("entry %d path = %s, want %s", i, e.Path, want[i])
		}
	}
	if entries[0].SHA256 != sha("alpha") {
		t.Errorf("sha = %s", entries[0].SHA256)
	}

	sums, err := os.ReadFile(filepath.Join(tape, manifest.SumsName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sums), sha("bravo")+"  downloads/sub/b.tar\n") {
		t.Errorf("SHA256SUMS = %q", sums)
	}
}

func TestPutPreservesMTime(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	a, _ := os.Stat(filepath.Join(src, "a.iso"))
	b, _ := os.Stat(filepath.Join(tape, "downloads", "a.iso"))
	if !a.ModTime().Equal(b.ModTime()) {
		t.Errorf("mtime %v != %v", b.ModTime(), a.ModTime())
	}
}

func TestPutRerunSkipsArchived(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	writeFile(t, filepath.Join(src, "new"), "charlie")

	sum := put(t, src, tape)
	if sum.Files != 1 || sum.Skipped != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	entries, _ := loadEntries(tape)
	if len(entries) != 4 {
		t.Fatalf("got %d entries", len(entries))
	}
}

func TestPutRefusesChangedSource(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	writeFile(t, filepath.Join(src, "a.iso"), "alpha v2")

	_, err := Put(opts(t, src, tape))
	if err == nil || !strings.Contains(err.Error(), "different version") {
		t.Fatalf("err = %v", err)
	}
}

func TestPutReplacesStalePartial(t *testing.T) {
	src, tape := setup(t)
	writeFile(t, filepath.Join(tape, "downloads", "a.iso"+PartialSuffix), "garbage")
	put(t, src, tape)
	if _, err := os.Stat(filepath.Join(tape, "downloads", "a.iso"+PartialSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial file left behind: %v", err)
	}
	verify(t, tape)
}

func TestPutRefusesUnknownFileOnTape(t *testing.T) {
	src, tape := setup(t)
	writeFile(t, filepath.Join(tape, "downloads", "a.iso"), "something else")
	_, err := Put(opts(t, src, tape))
	if err == nil || !strings.Contains(err.Error(), "not in the manifest") {
		t.Fatalf("err = %v", err)
	}
}

func TestPutSingleFileWithDest(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, filepath.Join(src, "a.iso"), tape)
	o.Prefix = "isos/a.iso"
	_, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tape, "isos", "a.iso")); err != nil {
		t.Fatal(err)
	}
}

func TestPutRejectsEscapingDest(t *testing.T) {
	src, tape := setup(t)
	for _, p := range []string{"../x", "/abs", "..", ".tapemgr", ".tapemgr/x"} {
		o := opts(t, src, tape)
		o.Prefix = p
		if _, err := Put(o); err == nil {
			t.Errorf("prefix %q accepted", p)
		}
	}
}

func TestVerifyOK(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	res := verify(t, tape)
	if res.Files != 3 || res.Verified != 3 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestVerifyDetectsCorruptionAndMissing(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	writeFile(t, filepath.Join(tape, "downloads", "a.iso"), "alphA")
	os.Remove(filepath.Join(tape, "downloads", "sub", "b.tar"))

	res := verify(t, tape)
	if res.Verified != 1 || res.Failed != 2 {
		t.Fatalf("result = %+v", res)
	}
}

func TestVerifyPrefix(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	res, err := Verify(VerifyOptions{TapeRoot: tape, Prefix: "downloads/sub", Log: io.Discard})
	if err != nil || res.Files != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	_, err = Verify(VerifyOptions{TapeRoot: tape, Prefix: "downloads/su", Log: io.Discard})
	if !errors.Is(err, ErrNoEntries) {
		t.Fatalf("partial name prefix matched: %v", err)
	}
}

func TestVerifyEmptyTape(t *testing.T) {
	_, err := Verify(VerifyOptions{TapeRoot: t.TempDir(), Log: io.Discard})
	if !errors.Is(err, ErrNoEntries) {
		t.Fatalf("err = %v", err)
	}
}
