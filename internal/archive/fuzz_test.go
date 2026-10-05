package archive

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/manifest"
)

func fuzzRecord(path string) pendingRecord {
	return pendingRecord{Entry: manifest.Entry{Path: path, Size: 1, SHA256: strings.Repeat("a", 64), MTime: time.Unix(1, 0).UTC()}}
}

func fuzzRecordLine(t testing.TB, path string) []byte {
	b, err := json.Marshal(fuzzRecord(path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The pending log is the only local copy of records for files already on
// tape. Whatever a crash left behind, every record openPending accepts must
// survive the next append and reopen.
func FuzzPendingLog(f *testing.F) {
	good := fuzzRecordLine(f, "a")
	f.Add(append(append([]byte{}, good...), '\n'))
	f.Add(good) // complete record, newline lost
	f.Add(append(append(append([]byte{}, good...), '\n'), good[:len(good)/2]...))
	f.Add([]byte("\n\n"))
	f.Fuzz(func(t *testing.T, content []byte) {
		name := filepath.Join(t.TempDir(), "p.jsonl")
		if err := os.WriteFile(name, content, 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := openPending(name)
		if err != nil {
			return // refusing a damaged log is allowed; losing records is not
		}
		before := len(p.records)
		if err := p.add(fuzzRecord("zz-new")); err != nil {
			t.Fatal(err)
		}
		p.close()
		p, err = openPending(name)
		if err != nil {
			t.Fatalf("log unreadable after an append: %v", err)
		}
		defer p.close()
		if len(p.records) != before+1 || p.records[before].Entry.Path != "zz-new" {
			t.Fatalf("had %d records, after one append %d", before, len(p.records))
		}
	})
}

// Resume journals are local but may be damaged. Loading one and picking a
// resume point must never panic, allocate without bound, or return a point
// outside the partial file.
func FuzzJournalResume(f *testing.F) {
	hdr, _ := json.Marshal(journalHeader{Source: "/s", Path: "p", Size: 100, ChunkSize: 16})
	rec, _ := json.Marshal(journalRecord{Offset: 32, Chunks: []string{sha(strings.Repeat("x", 16)), sha(strings.Repeat("x", 16))}})
	f.Add(append(append(hdr, '\n'), rec...), []byte(strings.Repeat("x", 40)))
	f.Add([]byte("{}\n{\"offset\":-5}\n{\"offset\":9223372036854775807}"), []byte("x"))
	f.Fuzz(func(t *testing.T, journal, partial []byte) {
		dir := t.TempDir()
		jname := filepath.Join(dir, "j.jsonl")
		os.WriteFile(jname, journal, 0o600)
		os.WriteFile(filepath.Join(dir, "part"), partial, 0o644)
		h, points, err := loadJournal(jname)
		if err != nil || h == nil {
			return
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		for _, cs := range []int64{16, 4096} {
			p, err := pickResumePoint(root, "part", points, 100, cs, nil)
			if err != nil || p == nil {
				continue
			}
			if p.offset <= 0 || p.offset > int64(len(partial)) || p.offset > 100 {
				t.Fatalf("resume point %d outside the partial file of %d bytes", p.offset, len(partial))
			}
		}
	})
}

// Small parity layouts are bounded: a crafted record must not make a scan
// allocate more than the limits allow.
func FuzzPendingRecordJSON(f *testing.F) {
	f.Add(append(fuzzRecordLine(f, "a"), '\n'))
	f.Fuzz(func(t *testing.T, b []byte) {
		recs, _, err := manifest.ReadJSONLLenient[pendingRecord](bytes.NewReader(b))
		if err != nil {
			return
		}
		for _, r := range recs {
			if r.Parity != nil && (r.Parity.Layout.ParitySize() < 0 || r.Parity.Layout.ParitySize() > 1<<55) {
				t.Fatalf("parity size %d", r.Parity.Layout.ParitySize())
			}
			if s := r.Entry.Stored(); s.Size < 0 {
				t.Fatalf("stored size %d", s.Size)
			}
		}
	})
}
