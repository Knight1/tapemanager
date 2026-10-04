// Package manifest reads and appends the per-tape integrity records.
//
// Everything lives on the tape itself so the cartridge stays self-describing:
//
//	SHA256SUMS                 sha256sum compatible list of files on this tape
//	.tapemgr/volume.json       tape identity
//	.tapemgr/manifest.jsonl    one Entry per archived file, in write order
//	.tapemgr/chunks.jsonl      per-file chunk hashes, in write order
package manifest

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Dir is the metadata directory relative to the tape root.
	Dir = ".tapemgr"
	// FileName is the JSON Lines manifest inside Dir.
	FileName = "manifest.jsonl"
	// ChunksName is the JSON Lines chunk hash list inside Dir.
	ChunksName = "chunks.jsonl"
	// VolumeName is the tape identity file inside Dir.
	VolumeName = "volume.json"
	// SumsName is the sha256sum compatible file at the tape root.
	SumsName = "SHA256SUMS"

	// MinChunkSize and MaxChunkSize bound the chunk size accepted from a
	// tape, so a damaged or crafted record cannot force a huge allocation.
	MinChunkSize = 16
	MaxChunkSize = 256 << 20

	maxVolumeFileSize = 64 << 10
)

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

func (e Entry) validate() error {
	if !ValidPath(e.Path) {
		return fmt.Errorf("invalid path %q", e.Path)
	}
	if e.Size < 0 {
		return fmt.Errorf("%s: negative size", e.Path)
	}
	if len(e.SHA256) != 64 || strings.Trim(e.SHA256, "0123456789abcdef") != "" {
		return fmt.Errorf("%s: invalid SHA-256 %q", e.Path, e.SHA256)
	}
	if e.Ref != nil && (!ValidID(e.Ref.Tape) || !ValidPath(e.Ref.Path)) {
		return fmt.Errorf("%s: invalid reference", e.Path)
	}
	return nil
}

func (c Chunks) validate() error {
	if !ValidPath(c.Path) {
		return fmt.Errorf("invalid path %q", c.Path)
	}
	if c.ChunkSize < MinChunkSize || c.ChunkSize > MaxChunkSize {
		return fmt.Errorf("%s: chunk size %d out of range", c.Path, c.ChunkSize)
	}
	return nil
}

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

// Volume identifies a tape.
type Volume struct {
	ID       string    `json:"id"`
	Label    string    `json:"label,omitempty"`
	LTFSUUID string    `json:"ltfs_uuid,omitempty"`
	Created  time.Time `json:"created"`
}

// Load reads all entries from the manifest under tapeRoot.
// A missing manifest yields no entries and no error.
func Load(tapeRoot string) ([]Entry, error) {
	return loadJSONL[Entry](filepath.Join(tapeRoot, Dir, FileName))
}

type validator interface{ validate() error }

// LoadChunks reads all chunk records under tapeRoot, keyed by path.
func LoadChunks(tapeRoot string) (map[string]Chunks, error) {
	list, err := loadJSONL[Chunks](filepath.Join(tapeRoot, Dir, ChunksName))
	if err != nil {
		return nil, err
	}
	m := make(map[string]Chunks, len(list))
	for _, c := range list {
		m[c.Path] = c
	}
	return m, nil
}

// Read parses JSON Lines entries from r.
func Read(r io.Reader) ([]Entry, error) {
	return readJSONL[Entry](r)
}

func loadJSONL[T validator](name string) ([]T, error) {
	f, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	list, err := readJSONL[T](f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return list, nil
}

// readJSONL parses and validates records. Manifests come from tapes and are
// treated as untrusted input.
func readJSONL[T validator](r io.Reader) ([]T, error) {
	var list []T
	sc := bufio.NewScanner(r)
	// Chunk lists of very large files make long lines.
	sc.Buffer(make([]byte, 64*1024), 256*1024*1024)
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
		if err := v.validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		list = append(list, v)
	}
	return list, sc.Err()
}

// LoadVolume reads the tape identity. A missing file yields nil.
func LoadVolume(tapeRoot string) (*Volume, error) {
	f, err := os.Open(filepath.Join(tapeRoot, Dir, VolumeName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxVolumeFileSize))
	if err != nil {
		return nil, err
	}
	var v Volume
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", VolumeName, err)
	}
	if !ValidID(v.ID) {
		return nil, fmt.Errorf("%s: invalid volume ID %q", VolumeName, v.ID)
	}
	return &v, nil
}

// InitVolume returns the tape identity, creating it on first use.
func InitVolume(tapeRoot, label, ltfsUUID string) (*Volume, error) {
	v, err := LoadVolume(tapeRoot)
	if err != nil || v != nil {
		return v, err
	}
	v = &Volume{ID: newID(), Label: label, LTFSUUID: ltfsUUID, Created: time.Now().UTC()}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(tapeRoot, Dir), 0o755); err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(filepath.Join(tapeRoot, Dir, VolumeName), append(b, '\n')); err != nil {
		return nil, err
	}
	return v, nil
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// WriteFileAtomic replaces name with data so readers see either the old or
// the new content, never a partial file.
func WriteFileAtomic(name string, data []byte) error {
	tmp := name + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
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
	return os.Rename(tmp, name)
}

// Writer appends records to the manifest files on a tape.
type Writer struct {
	manifest *os.File
	chunks   *os.File
	sums     *os.File
}

// OpenWriter opens (creating if needed) the manifest files under tapeRoot
// for appending.
func OpenWriter(tapeRoot string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Join(tapeRoot, Dir), 0o755); err != nil {
		return nil, err
	}
	const flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	var w Writer
	var err error
	for _, f := range []struct {
		dst  **os.File
		name string
	}{
		{&w.manifest, filepath.Join(tapeRoot, Dir, FileName)},
		{&w.chunks, filepath.Join(tapeRoot, Dir, ChunksName)},
		{&w.sums, filepath.Join(tapeRoot, SumsName)},
	} {
		if *f.dst, err = os.OpenFile(f.name, flags, 0o644); err != nil {
			w.Close()
			return nil, err
		}
	}
	return &w, nil
}

// Append records e durably, with its chunk hashes if c is not nil. It is
// called only after the file data itself has been synced, so an entry never
// points at incomplete data. The manifest entry is written last because it
// is what marks a file as archived.
func (w *Writer) Append(e Entry, c *Chunks) error {
	if c != nil {
		if err := appendJSON(w.chunks, c); err != nil {
			return err
		}
	}
	if e.Ref == nil {
		if _, err := fmt.Fprintf(w.sums, "%s  %s\n", e.SHA256, e.Path); err != nil {
			return err
		}
		if err := w.sums.Sync(); err != nil {
			return err
		}
	}
	return appendJSON(w.manifest, e)
}

func appendJSON(f *os.File, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Close closes all files.
func (w *Writer) Close() error {
	var errs []error
	for _, f := range []*os.File{w.manifest, w.chunks, w.sums} {
		if f != nil {
			errs = append(errs, f.Close())
		}
	}
	return errors.Join(errs...)
}
