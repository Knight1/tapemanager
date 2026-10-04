// Package manifest defines the per-tape integrity records and reads and
// writes them on a tape.
//
// Everything lives on the tape itself so the cartridge stays self-describing:
//
//	SHA256SUMS                                sha256sum compatible list of files
//	.tapemgr/volume.json                      tape identity
//	.tapemgr/segments/NNNNNN.manifest.jsonl   one Entry per archived file
//	.tapemgr/segments/NNNNNN.chunks.jsonl     chunk hashes for those files
//	.tapemgr/segments/NNNNNN.parity           Reed-Solomon parity data
//	.tapemgr/segments/NNNNNN.parity.jsonl     parity layout per file
//
// Tape is append-only: every write, even to an existing file, lands at the
// end of the recorded data. Appending one line per archived file would
// scatter the manifest into thousands of small extents between the data and
// make reading it back a seek per line. Records are therefore collected
// locally and written in large segments, each a new file written in one go.
package manifest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/Knight1/tapemanager/internal/parity"
)

const (
	// Dir is the metadata directory relative to the tape root.
	Dir = ".tapemgr"
	// SegmentsDir holds the manifest segments inside Dir.
	SegmentsDir = "segments"
	// VolumeName is the tape identity file inside Dir.
	VolumeName = "volume.json"
	// SumsName is the sha256sum compatible file at the tape root.
	SumsName = "SHA256SUMS"

	// MinChunkSize and MaxChunkSize bound the chunk size accepted from a
	// tape, so a damaged or crafted record cannot force a huge allocation.
	MinChunkSize = 16
	MaxChunkSize = 256 << 20

	maxVolumeFileSize = 64 << 10
	// maxLine bounds a single JSON line. Chunk lists of very large files
	// make long lines: 4 TiB in 4 MiB chunks is about 70 MB.
	maxLine = 256 << 20
)

// Entry describes one archived file.
type Entry struct {
	Path       string    `json:"path"` // relative to the tape root, slash separated
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	MTime      time.Time `json:"mtime"`
	Source     string    `json:"source"`
	ArchivedAt time.Time `json:"archived_at"`
	// Ref is set when the content was not written because an identical
	// file already exists on tape. The entry then only records the source.
	Ref *Ref `json:"ref,omitempty"`
	// Recovered is set for files found on tape without a manifest record
	// and recorded by 'archive recover'. Their hash was computed from the
	// tape, not from the source, and their source is unknown.
	Recovered bool `json:"recovered,omitempty"`
}

// Ref points at the copy of a deduplicated file.
type Ref struct {
	Tape string `json:"tape"` // volume ID
	Path string `json:"path"`
}

// Chunks holds the SHA-256 of each fixed-size piece of a file, so damage can
// be located to a byte range.
type Chunks struct {
	Path      string   `json:"path"`
	ChunkSize int64    `json:"chunk_size"`
	SHA256    []string `json:"sha256"`
}

// Parity records where a file's parity is stored and how it is laid out.
type Parity struct {
	Path   string        `json:"path"`
	Layout parity.Layout `json:"layout"`
	// Offset of the file's parity in the segment's parity file. Set when
	// the segment is written.
	Offset int64 `json:"offset"`
	// Hashes of every parity shard, so damaged parity is never used.
	Hashes []string `json:"hashes"`
	// DataHashes of the data shards, for small files only. Large files use
	// their chunk hashes.
	DataHashes []string `json:"data_hashes,omitempty"`

	// Segment holding the parity data, set when read from tape.
	Segment int `json:"-"`
}

// maxParityFileSize bounds layouts read from tape so offset arithmetic
// cannot overflow.
const maxParityFileSize = 1 << 50

// Validate checks a parity record read from untrusted input.
func (p Parity) Validate() error {
	if !ValidPath(p.Path) {
		return fmt.Errorf("invalid path %q", p.Path)
	}
	if err := p.Layout.Validate(); err != nil {
		return fmt.Errorf("%s: %w", p.Path, err)
	}
	if p.Layout.Size > maxParityFileSize {
		return fmt.Errorf("%s: parity layout too large", p.Path)
	}
	if p.Offset < 0 || p.Offset > maxParityFileSize {
		return fmt.Errorf("%s: parity offset out of range", p.Path)
	}
	if int64(len(p.Hashes)) != p.Layout.ParityShards() {
		return fmt.Errorf("%s: %d parity hashes, layout needs %d", p.Path, len(p.Hashes), p.Layout.ParityShards())
	}
	want := 0
	if p.Layout.Scheme == parity.SchemeSmall {
		want = p.Layout.K
	}
	if len(p.DataHashes) != want {
		return fmt.Errorf("%s: wrong number of data shard hashes", p.Path)
	}
	for _, list := range [][]string{p.Hashes, p.DataHashes} {
		for _, h := range list {
			if !validSHA256(h) {
				return fmt.Errorf("%s: invalid parity hash", p.Path)
			}
		}
	}
	return nil
}

// Volume identifies a tape.
type Volume struct {
	ID       string    `json:"id"`
	Label    string    `json:"label,omitempty"`
	LTFSUUID string    `json:"ltfs_uuid,omitempty"`
	Created  time.Time `json:"created"`
}

// ValidPath reports whether p is a clean, relative, slash separated path
// that stays inside the tape root and does not touch tapemgr metadata.
func ValidPath(p string) bool {
	return p != "" && p != "." && p != ".." && path.Clean(p) == p && !path.IsAbs(p) &&
		!strings.HasPrefix(p, "../") && !strings.ContainsAny(p, "\x00\n\r") &&
		p != Dir && !strings.HasPrefix(p, Dir+"/") && p != SumsName
}

// ValidID reports whether id has the form of a volume ID (a UUID). IDs
// become file names in the catalog, so nothing else is accepted.
func ValidID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'):
		default:
			return false
		}
	}
	return true
}

func validSHA256(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// Validate checks an entry read from untrusted input.
func (e Entry) Validate() error {
	if !ValidPath(e.Path) {
		return fmt.Errorf("invalid path %q", e.Path)
	}
	if e.Size < 0 {
		return fmt.Errorf("%s: negative size", e.Path)
	}
	if !validSHA256(e.SHA256) {
		return fmt.Errorf("%s: invalid SHA-256 %q", e.Path, e.SHA256)
	}
	if e.Ref != nil && (!ValidID(e.Ref.Tape) || !ValidPath(e.Ref.Path)) {
		return fmt.Errorf("%s: invalid reference", e.Path)
	}
	return nil
}

// Validate checks a chunk record read from untrusted input.
func (c Chunks) Validate() error {
	if !ValidPath(c.Path) {
		return fmt.Errorf("invalid path %q", c.Path)
	}
	if c.ChunkSize < MinChunkSize || c.ChunkSize > MaxChunkSize {
		return fmt.Errorf("%s: chunk size %d out of range", c.Path, c.ChunkSize)
	}
	for _, s := range c.SHA256 {
		if !validSHA256(s) {
			return fmt.Errorf("%s: invalid chunk hash", c.Path)
		}
	}
	return nil
}

// Validator is a record that can check itself after decoding.
type Validator interface{ Validate() error }

// Read parses manifest entries from r.
func Read(r io.Reader) ([]Entry, error) {
	return ReadJSONL[Entry](r)
}

// ReadJSONL parses and validates JSON Lines records. Everything read from a
// tape or a local state file is treated as untrusted input.
func ReadJSONL[T Validator](r io.Reader) ([]T, error) {
	var list []T
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if err := v.Validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		list = append(list, v)
	}
	return list, sc.Err()
}

// MarshalJSONL encodes records as JSON Lines.
func MarshalJSONL[T any](list []T) ([]byte, error) {
	var b strings.Builder
	for _, v := range list {
		line, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// WriteFileAtomic replaces the local file name with data so readers see
// either the old or the new content, never a partial file.
func WriteFileAtomic(name string, data []byte) error {
	tmp := name + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	return finishAtomic(f, data, func() error { return os.Rename(tmp, name) })
}

func finishAtomic(f *os.File, data []byte, rename func() error) error {
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return rename()
}
