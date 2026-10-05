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

	"github.com/Knight1/tapemanager/internal/agestream"
	"github.com/Knight1/tapemanager/internal/manifest"
)

// A journal records the progress of one file transfer so an interrupted
// write can continue from the last checkpoint instead of from the start.
//
// The first line is a journalHeader, every further line a journalRecord.
// Records are appended and synced only after the file data up to their
// offset has been synced to tape.
type journalHeader struct {
	Source string    `json:"source"`
	Path   string    `json:"path"`
	Size   int64     `json:"size"`
	MTime  time.Time `json:"mtime"`
	// Change is the source's changeID: a rewrite that kept the mtime must
	// not be resumed into the old transfer.
	Change    string `json:"change,omitempty"`
	ChunkSize int64  `json:"chunk_size"`
	ParityM   int    `json:"parity_m,omitempty"`
	// Age holds the header, nonce and file key of an encrypted file, so an
	// interrupted write can continue the same encryption. The file key is
	// secret; journals are only readable by their owner and are deleted
	// once the file is recorded.
	Age        *agestream.Params `json:"age,omitempty"`
	Recipients []string          `json:"recipients,omitempty"`
}

type journalRecord struct {
	Offset int64    `json:"offset"`
	State  []byte   `json:"state"`            // marshaled SHA-256 state at Offset
	Chunks []string `json:"chunks"`           // chunk hashes added since the previous record
	SHA256 string   `json:"sha256,omitempty"` // set on the final record only
	// Parity shard hashes added since the previous record.
	ParityHashes []string `json:"parity_hashes,omitempty"`
	// Parity is the complete parity record, on the final record only.
	Parity *manifest.Parity `json:"parity,omitempty"`
	// For encrypted files: the plaintext hash state at Offset and the
	// payload chunk that follows, and on the final record the SHA-256 of
	// the encrypted file (SHA256 then holds the plaintext's).
	PlainState   []byte `json:"plain_state,omitempty"`
	PlainChunk   int64  `json:"plain_chunk,omitempty"`
	StoredSHA256 string `json:"stored_sha256,omitempty"`
}

// resumePoint is a position a transfer can continue from.
type resumePoint struct {
	offset int64
	state  []byte
	chunks []string // all chunk hashes up to offset
	sha256 string   // final file hash if the transfer completed
	// parityHashes holds all parity shard hashes up to offset.
	parityHashes []string
	parity       *manifest.Parity // final record only
	plainState   []byte           // encrypted files
	plainChunk   int64
	storedSHA256 string // final record of an encrypted file
}

// fileKey names the per-file state (journal, staged parity) of rel.
func fileKey(rel string) string {
	h := sha256.Sum256([]byte(rel))
	return hex.EncodeToString(h[:16])
}

func journalPath(dir, rel string) string {
	return filepath.Join(dir, fileKey(rel)+".jsonl")
}

type journal struct {
	f              *os.File
	recorded       int // number of chunk hashes already in the journal
	parityRecorded int // number of parity hashes already in the journal
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
		b, err := json.Marshal(journalRecord{Offset: start.offset, State: start.state, Chunks: start.chunks, SHA256: start.sha256, ParityHashes: start.parityHashes, Parity: start.parity,
			PlainState: start.plainState, PlainChunk: start.plainChunk, StoredSHA256: start.storedSHA256})
		if err != nil {
			return nil, err
		}
		data = append(data, append(b, '\n')...)
	}
	// Owner only: the journal of an encrypted file holds its file key.
	if err := manifest.WriteFileAtomicPerm(name, data, 0o600); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	j := &journal{f: f}
	if start != nil {
		j.recorded = len(start.chunks)
		j.parityRecorded = len(start.parityHashes)
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
	var chunks, parityHashes []string
	for sc.Scan() {
		var r journalRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			break
		}
		if r.Parity != nil && r.Parity.Validate() != nil {
			break
		}
		chunks = append(chunks, r.Chunks...)
		parityHashes = append(parityHashes, r.ParityHashes...)
		points = append(points, resumePoint{
			offset:       r.Offset,
			state:        r.State,
			chunks:       chunks[:len(chunks):len(chunks)],
			sha256:       r.SHA256,
			parityHashes: parityHashes[:len(parityHashes):len(parityHashes)],
			parity:       r.Parity,
			plainState:   r.PlainState,
			plainChunk:   r.PlainChunk,
			storedSHA256: r.StoredSHA256,
		})
	}
	return &hdr, points, sc.Err()
}
