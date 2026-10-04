package manifest

import (
	"github.com/Knight1/tapemanager/internal/parity"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTape(t *testing.T, dir string) *Tape {
	t.Helper()
	tp, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tp.Close() })
	return tp
}

func sha(c string) string { return strings.Repeat(c, 64) }

func TestSegmentsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tp := openTape(t, dir)
	mtime := time.Date(2026, 10, 3, 12, 0, 0, 123, time.UTC)
	seg1 := []Entry{
		{Path: "a/b.iso", Size: 42, SHA256: sha("a"), MTime: mtime},
		{Path: "c d", Size: 0, SHA256: sha("c"), MTime: mtime},
	}
	ch1 := []Chunks{{Path: "a/b.iso", ChunkSize: 16, SHA256: []string{sha("1"), sha("2"), sha("3")}}}
	if err := tp.WriteSegment(Segment{Entries: seg1, Chunks: ch1}, seg1); err != nil {
		t.Fatal(err)
	}
	seg2 := []Entry{{Path: "e", Size: 1, SHA256: sha("e"), Ref: &Ref{Tape: "16df0f97-5090-44fe-ba22-7ee000810811", Path: "x"}}}
	all := append(append([]Entry{}, seg1...), seg2...)
	if err := tp.WriteSegment(Segment{Entries: seg2}, all); err != nil {
		t.Fatal(err)
	}
	// An empty flush writes nothing.
	if err := tp.WriteSegment(Segment{}, all); err != nil {
		t.Fatal(err)
	}

	out, err := tp.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[1].Path != "c d" || out[2].Path != "e" || !out[0].MTime.Equal(mtime) {
		t.Fatalf("got %+v", out)
	}
	chunks, err := tp.Chunks()
	if err != nil || len(chunks["a/b.iso"].SHA256) != 3 {
		t.Fatalf("chunks = %+v, %v", chunks, err)
	}
	names, _ := filepath.Glob(filepath.Join(dir, Dir, SegmentsDir, "*"))
	if len(names) != 4 {
		t.Errorf("segment files = %v", names)
	}
	sums, _ := os.ReadFile(filepath.Join(dir, SumsName))
	if string(sums) != sha("a")+"  a/b.iso\n"+sha("c")+"  c d\n" {
		t.Errorf("SHA256SUMS = %q", sums)
	}
}

// Leftover temp files and unrelated names in the segment directory, for
// example from a crash during a segment write, are ignored.
func TestSegmentsIgnoreIncomplete(t *testing.T) {
	dir := t.TempDir()
	tp := openTape(t, dir)
	e := []Entry{{Path: "a", Size: 1, SHA256: sha("a")}}
	tp.WriteSegment(Segment{Entries: e}, e)
	seg := filepath.Join(dir, Dir, SegmentsDir)
	for _, n := range []string{"000002.manifest.jsonl.tmp", "000003.chunks.jsonl", "junk", "1.manifest.jsonl", "000000.manifest.jsonl", "+00001.manifest.jsonl", "-00001.manifest.jsonl"} {
		os.WriteFile(filepath.Join(seg, n), []byte("garbage\n"), 0o644)
	}
	out, err := tp.Entries()
	if err != nil || len(out) != 1 {
		t.Fatalf("got %+v, %v", out, err)
	}
	// The next segment number follows the highest complete one.
	tp.WriteSegment(Segment{Entries: []Entry{{Path: "b", Size: 1, SHA256: sha("b")}}}, nil)
	if _, err := os.Stat(filepath.Join(seg, "000002.manifest.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestEntriesMissing(t *testing.T) {
	out, err := openTape(t, t.TempDir()).Entries()
	if err != nil || out != nil {
		t.Fatalf("got %v, %v", out, err)
	}
}

func TestSegmentDirSymlinkOutOfTape(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "tape")
	outside := filepath.Join(base, "outside")
	os.MkdirAll(filepath.Join(dir, Dir), 0o755)
	os.MkdirAll(outside, 0o755)
	os.Symlink(outside, filepath.Join(dir, Dir, SegmentsDir))
	e := []Entry{{Path: "a", Size: 1, SHA256: sha("a")}}
	if err := openTape(t, dir).WriteSegment(Segment{Entries: e}, e); err == nil {
		t.Fatal("segment written through a symlink out of the tape")
	}
	if names, _ := os.ReadDir(outside); len(names) != 0 {
		t.Fatalf("files outside the tape: %v", names)
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
	os.MkdirAll(filepath.Join(dir, Dir, SegmentsDir), 0o755)
	os.WriteFile(filepath.Join(dir, Dir, SegmentsDir, "000001.manifest.jsonl"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, Dir, SegmentsDir, "000001.chunks.jsonl"), []byte(`{"path":"a","chunk_size":4611686018427387904,"sha256":[]}`+"\n"), 0o644)
	if _, err := openTape(t, dir).Chunks(); err == nil {
		t.Fatal("huge chunk size accepted")
	}
}

func TestLoadVolumeRejectsBadID(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, Dir), 0o755)
	os.WriteFile(filepath.Join(dir, Dir, VolumeName), []byte(`{"id":"../../../tmp/evil"}`), 0o644)
	if _, err := openTape(t, dir).Volume(); err == nil {
		t.Fatal("bad volume ID accepted")
	}
}

func TestInitVolumeIsStable(t *testing.T) {
	dir := t.TempDir()
	tp := openTape(t, dir)
	a, err := tp.InitVolume("L1", "")
	if err != nil || !ValidID(a.ID) {
		t.Fatalf("%+v, %v", a, err)
	}
	b, _ := tp.InitVolume("other", "")
	if b.ID != a.ID || b.Label != "L1" {
		t.Fatalf("volume changed: %+v", b)
	}
}

func TestParityValidate(t *testing.T) {
	l := parity.ForFile(200, 16, 2)
	good := Parity{Path: "a", Layout: *l, Hashes: []string{sha("1"), sha("2")}, DataHashes: make([]string, parity.K)}
	for i := range good.DataHashes {
		good.DataHashes[i] = sha("d")
	}
	if l.Scheme != parity.SchemeSmall {
		t.Fatalf("scheme %s", l.Scheme)
	}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(p *Parity){
		func(p *Parity) { p.Path = "../x" },
		func(p *Parity) { p.Offset = -1 },
		func(p *Parity) { p.Hashes = p.Hashes[:1] },
		func(p *Parity) { p.Hashes[0] = "nope" },
		func(p *Parity) { p.DataHashes = nil },
		func(p *Parity) { p.Layout.Scheme = "xor" },
		func(p *Parity) { p.Layout.Size = 1 << 60; p.Layout.ShardSize = (1<<60 + 19) / 20 },
	}
	for i, mutate := range bad {
		p := good
		p.Hashes = append([]string(nil), good.Hashes...)
		p.DataHashes = append([]string(nil), good.DataHashes...)
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestReadParityShardBounds(t *testing.T) {
	tp := openTape(t, t.TempDir())
	l := parity.ForFile(200, 16, 1)
	p := Parity{Path: "a", Layout: *l, Segment: 1}
	for _, i := range []int64{-1, 1, 1 << 40} {
		if _, err := tp.ReadParityShard(p, i); err == nil {
			t.Errorf("shard %d accepted", i)
		}
	}
}
