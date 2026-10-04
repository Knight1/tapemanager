// Package manifest reads and appends the per-tape integrity manifest.
//
// The manifest lives on the tape itself so the cartridge stays
// self-describing. It is a JSON Lines file with one entry per archived file,
// appended in write order. A plain SHA256SUMS file is kept next to it so the
// tape can be checked with standard tools (`sha256sum -c`) as well.
package manifest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	// Dir is the metadata directory relative to the tape root.
	Dir = ".tapemgr"
	// FileName is the JSON Lines manifest inside Dir.
	FileName = "manifest.jsonl"
	// SumsName is the sha256sum compatible file at the tape root.
	SumsName = "SHA256SUMS"
)

// Entry describes one archived file.
type Entry struct {
	Path       string    `json:"path"` // relative to the tape root, slash separated
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	MTime      time.Time `json:"mtime"`
	Source     string    `json:"source"`
	ArchivedAt time.Time `json:"archived_at"`
}

// Load reads all entries from the manifest under tapeRoot.
// A missing manifest yields no entries and no error.
func Load(tapeRoot string) ([]Entry, error) {
	f, err := os.Open(filepath.Join(tapeRoot, Dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// Read parses JSON Lines entries from r.
func Read(r io.Reader) ([]Entry, error) {
	var entries []Entry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, fmt.Errorf("manifest line %d: %w", line, err)
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
}

// Writer appends entries to the manifest and SHA256SUMS on a tape.
type Writer struct {
	manifest *os.File
	sums     *os.File
}

// OpenWriter opens (creating if needed) the manifest files under tapeRoot
// for appending.
func OpenWriter(tapeRoot string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Join(tapeRoot, Dir), 0o755); err != nil {
		return nil, err
	}
	const flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	m, err := os.OpenFile(filepath.Join(tapeRoot, Dir, FileName), flags, 0o644)
	if err != nil {
		return nil, err
	}
	s, err := os.OpenFile(filepath.Join(tapeRoot, SumsName), flags, 0o644)
	if err != nil {
		m.Close()
		return nil, err
	}
	return &Writer{manifest: m, sums: s}, nil
}

// Append records e durably. It is called only after the file data itself
// has been synced, so an entry never points at incomplete data.
func (w *Writer) Append(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := w.manifest.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := w.manifest.Sync(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.sums, "%s  %s\n", e.SHA256, e.Path); err != nil {
		return err
	}
	return w.sums.Sync()
}

// Close closes both files.
func (w *Writer) Close() error {
	return errors.Join(w.manifest.Close(), w.sums.Close())
}
