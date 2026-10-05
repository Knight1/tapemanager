package manifest

import (
	"bytes"
	"testing"
)

// Manifest lines come from tape and are untrusted: parsing and validation
// must never panic, and a validated entry must describe a sane tape file.
func FuzzEntryJSON(f *testing.F) {
	f.Add([]byte(`{"path":"a","size":5,"sha256":"` + string(bytes.Repeat([]byte("a"), 64)) + `","mtime":"2026-01-01T00:00:00Z"}` + "\n"))
	f.Add([]byte(`{"path":"a","size":5,"sha256":"` + string(bytes.Repeat([]byte("a"), 64)) + `","mtime":"2026-01-01T00:00:00Z","age":{"size":300,"sha256":"` + string(bytes.Repeat([]byte("b"), 64)) + `","recipients":["age1x"]}}` + "\n"))
	f.Add([]byte("garbage\n{\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		entries, _, err := ReadJSONLLenient[Entry](bytes.NewReader(b))
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.Validate() != nil {
				t.Fatalf("lenient reader returned an invalid entry: %+v", e)
			}
			s := e.Stored()
			if !ValidPath(s.Path) || s.Size < e.Size || s.Size < 0 {
				t.Fatalf("bad stored view %+v of %+v", s, e)
			}
		}
	})
}
