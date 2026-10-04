package manifest

import (
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
		{Path: "a/b.iso", Size: 42, SHA256: "ab", MTime: mtime},
		{Path: "c d", Size: 0, SHA256: "cd", MTime: mtime},
	}
	for _, e := range in {
		if err := w.Append(e); err != nil {
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
	_, err := Read(strings.NewReader("{\"path\":\"x\"}\nnot json\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v", err)
	}
}
