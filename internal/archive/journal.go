package archive

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Knight1/tapemanager/internal/manifest"
)

// A journal records the progress of one file transfer so an interrupted
// write can continue from the last checkpoint instead of from the start.
//
// The first line is a journalHeader, every further line a journalRecord.
// Records are appended and synced only after the file data up to their
// offset has been synced to tape.
type journalHeader struct {
	Source    string    `json:"source"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	MTime     time.Time `json:"mtime"`
	ChunkSize int64     `json:"chunk_size"`
}

type journalRecord struct {
	Offset int64    `json:"offset"`
	State  []byte   `json:"state"`            // marshaled SHA-256 state at Offset
	Chunks []string `json:"chunks"`           // chunk hashes added since the previous record
	SHA256 string   `json:"sha256,omitempty"` // set on the final record only
}

// resumePoint is a position a transfer can continue from.
type resumePoint struct {
	offset int64
	state  []byte
	chunks []string // all chunk hashes up to offset
	sha256 string   // final file hash if the transfer completed
}

func journalPath(dir, rel string) string {
	h := sha256.Sum256([]byte(rel))
	return filepath.Join(dir, hex.EncodeToString(h[:16])+".jsonl")
}

type journal struct {
	f        *os.File
	recorded int // number of chunk hashes already in the journal
}

// writeJournal creates or replaces the journal at name, starting with hdr
// and an optional consolidated record, and opens it for appending.
func writeJournal(name string, hdr journalHeader, start *resumePoint) (*journal, error) {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return nil, err
	}
	b, err := json.Marshal(hdr)
	if err != nil {
		return nil, err
	}
	data := append(b, '\n')
	if start != nil {
		b, err := json.Marshal(journalRecord{Offset: start.offset, State: start.state, Chunks: start.chunks, SHA256: start.sha256})
		if err != nil {
			return nil, err
		}
		data = append(data, append(b, '\n')...)
	}
	if err := manifest.WriteFileAtomic(name, data); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	j := &journal{f: f}
	if start != nil {
		j.recorded = len(start.chunks)
	}
	return j, nil
}

func (j *journal) append(r journalRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *journal) close() error { return j.f.Close() }

// loadJournal reads the journal at name and returns its header and all
// resume points in order. A torn last line from a crash is ignored.
func loadJournal(name string) (*journalHeader, []resumePoint, error) {
	f, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 256*1024*1024)
	if !sc.Scan() {
		return nil, nil, sc.Err()
	}
	var hdr journalHeader
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil {
		return nil, nil, nil
	}
	var points []resumePoint
	var chunks []string
	for sc.Scan() {
		var r journalRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			break
		}
		chunks = append(chunks, r.Chunks...)
		points = append(points, resumePoint{offset: r.Offset, state: r.State, chunks: chunks[:len(chunks):len(chunks)], sha256: r.SHA256})
	}
	return &hdr, points, sc.Err()
}
