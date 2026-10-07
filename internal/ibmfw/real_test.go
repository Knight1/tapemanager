package ibmfw

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The vendor images are not part of the repository; when they are next to
// the source, check that every one of them passes.
func TestRealImages(t *testing.T) {
	files, _ := filepath.Glob("../../*.fmrz")
	if len(files) == 0 {
		t.Skip("no vendor images present")
	}
	keys, err := IBMKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		r, err := Inspect(b, keys, time.Now())
		if err != nil || !r.Valid() {
			t.Errorf("%s: %v %v", f, err, r.Problems)
			continue
		}
		t.Logf("%s: level %s, platform %s, %d sections, %d records, signed up to %#x of %#x", filepath.Base(f), r.Header.Level, r.Platform, len(r.Sections), len(r.Records), r.Signed, len(b))
	}
}
