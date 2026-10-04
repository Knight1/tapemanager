package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 10, 3, 12, 0, 0, 123, time.UTC)
	in := []Entry{
		{Path: "a/b.iso", Size: 42, SHA256: strings.Repeat("a", 64), MTime: mtime},
		{Path: "c d", Size: 0, SHA256: strings.Repeat("c", 64), MTime: mtime},
	}
	for _, e := range in {
		if err := w.Append(e, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Path != "c d" || !out[0].MTime.Equal(mtime) {
		t.Fatalf("got %+v", out)
	}
}

func TestLoadMissing(t *testing.T) {
	out, err := Load(t.TempDir())
	if err != nil || out != nil {
		t.Fatalf("got %v, %v", out, err)
	}
}

func TestReadBadLine(t *testing.T) {
	good := `{"path":"x","size":1,"sha256":"` + strings.Repeat("0", 64) + `"}`
	_, err := Read(strings.NewReader(good + "\nnot json\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidPath(t *testing.T) {
	good := []string{"a", "a/b.iso", "dir/.hidden", "x/SHA256SUMS", "x/.tapemgr"}
	bad := []string{"", ".", "..", "../a", "a/../../b", "/etc/passwd", "a//b", "a/", "a/./b",
		".tapemgr", ".tapemgr/manifest.jsonl", "SHA256SUMS", "a\x00b", "a\nb"}
	for _, p := range good {
		if !ValidPath(p) {
			t.Errorf("ValidPath(%q) = false", p)
		}
	}
	for _, p := range bad {
		if ValidPath(p) {
			t.Errorf("ValidPath(%q) = true", p)
		}
	}
}

func TestValidID(t *testing.T) {
	if !ValidID("16df0f97-5090-44fe-ba22-7ee000810811") {
		t.Error("valid ID rejected")
	}
	for _, id := range []string{"", "../../etc/passwd", "16df0f97-5090-44fe-ba22-7ee00081081/", "16DF0F97-5090-44FE-BA22-7EE000810811", "16df0f97x5090-44fe-ba22-7ee000810811"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
}

func TestReadRejectsUntrustedEntries(t *testing.T) {
	sha := strings.Repeat("0", 64)
	for _, line := range []string{
		`{"path":"../../etc/shadow","size":1,"sha256":"` + sha + `"}`,
		`{"path":"/etc/shadow","size":1,"sha256":"` + sha + `"}`,
		`{"path":"a","size":-1,"sha256":"` + sha + `"}`,
		`{"path":"a","size":1,"sha256":"zz"}`,
		`{"path":"a","size":1,"sha256":"` + sha + `","ref":{"tape":"../x","path":"a"}}`,
	} {
		if _, err := Read(strings.NewReader(line)); err == nil {
			t.Errorf("accepted %s", line)
		}
	}
}

func TestLoadChunksRejectsHugeChunkSize(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, Dir), 0o755)
	os.WriteFile(filepath.Join(dir, Dir, ChunksName), []byte(`{"path":"a","chunk_size":4611686018427387904,"sha256":[]}`+"\n"), 0o644)
	if _, err := LoadChunks(dir); err == nil {
		t.Fatal("huge chunk size accepted")
	}
}

func TestLoadVolumeRejectsBadID(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, Dir), 0o755)
	os.WriteFile(filepath.Join(dir, Dir, VolumeName), []byte(`{"id":"../../../tmp/evil"}`), 0o644)
	if _, err := LoadVolume(dir); err == nil {
		t.Fatal("bad volume ID accepted")
	}
}

func TestInitVolumeIsStable(t *testing.T) {
	dir := t.TempDir()
	a, err := InitVolume(dir, "L1", "")
	if err != nil || !ValidID(a.ID) {
		t.Fatalf("%+v, %v", a, err)
	}
	b, _ := InitVolume(dir, "other", "")
	if b.ID != a.ID || b.Label != "L1" {
		t.Fatalf("volume changed: %+v", b)
	}
}
