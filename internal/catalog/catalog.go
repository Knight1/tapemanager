// Package catalog keeps a local copy of every tape's manifest so tapes can be
// searched without loading them.
//
// The catalog is plain files and can always be rebuilt from the tapes:
//
//	<dir>/tapes/<id>.json      tape record and verification history
//	<dir>/tapes/<id>.jsonl     copy of the tape's manifest.jsonl
//	<dir>/journal/<id>/        resume journals for interrupted writes
//	<dir>/parity/<id>/         parity staged until the next segment write
//	<dir>/pending/<id>.jsonl   records not yet written to the tape
//	<dir>/written/<id>.jsonl   records this machine wrote to the tape
//	<dir>/continue/<key>.json  archive runs that filled a tape and continue on the next
//
// The manifest on the tape is authoritative. The catalog is updated after the
// tape, so a failed catalog write never loses archived data.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Knight1/tapemanager/internal/manifest"
)

// Tape is the local record of one tape.
type Tape struct {
	manifest.Volume
	Files         int            `json:"files"`
	Bytes         int64          `json:"bytes"`
	ImportedAt    time.Time      `json:"imported_at"`
	Verifications []Verification `json:"verifications,omitempty"`
}

// Verification records one run of archive verify.
type Verification struct {
	At         time.Time `json:"at"`
	Files      int       `json:"files"`
	Verified   int       `json:"verified"`
	Repairable int       `json:"repairable,omitempty"`
	Failed     int       `json:"failed"`
	Problems   int       `json:"problems,omitempty"` // damaged metadata or parity
}

// Passed reports whether the verification found the tape in full health.
func (v Verification) Passed() bool {
	return v.Failed == 0 && v.Repairable == 0 && v.Problems == 0
}

// LastVerified returns the most recent verification, or nil.
func (t *Tape) LastVerified() *Verification {
	if len(t.Verifications) == 0 {
		return nil
	}
	return &t.Verifications[len(t.Verifications)-1]
}

// VerifiedSince reports whether the tape's most recent verification passed
// and happened after t. A later failed verification revokes earlier passes.
// Damage that parity could still repair also counts as failed: the tape is
// degrading and should not be the only copy.
func (t *Tape) VerifiedSince(at time.Time) bool {
	v := t.LastVerified()
	return v != nil && v.Passed() && v.At.After(at)
}

// Hit is one search result.
type Hit struct {
	Tape  Tape
	Entry manifest.Entry
}

// Catalog is a catalog directory.
type Catalog struct {
	Dir string
}

// Open opens the catalog in dir, creating it if needed.
func Open(dir string) (*Catalog, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tapes"), 0o755); err != nil {
		return nil, err
	}
	return &Catalog{Dir: dir}, nil
}

func (c *Catalog) recordPath(id string) string   { return filepath.Join(c.Dir, "tapes", id+".json") }
func (c *Catalog) manifestPath(id string) string { return filepath.Join(c.Dir, "tapes", id+".jsonl") }

// PendingPath returns the pending manifest log of tape id.
func (c *Catalog) PendingPath(id string) string { return filepath.Join(c.Dir, "pending", id+".jsonl") }

func (c *Catalog) writtenPath(id string) string { return filepath.Join(c.Dir, "written", id+".jsonl") }

// RecordWritten notes entries that this machine archived to tape id. Only
// these are trusted as proof of where a source file went: a manifest read
// from a tape can claim any source path.
func (c *Catalog) RecordWritten(id string, entries []manifest.Entry) error {
	if !manifest.ValidID(id) {
		return fmt.Errorf("invalid tape ID %q", id)
	}
	if len(entries) == 0 {
		return nil
	}
	data, err := manifest.MarshalJSONL(entries)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(c.Dir, "written"), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(c.writtenPath(id), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if err := manifest.SyncDir(filepath.Dir(c.writtenPath(id))); err != nil {
		f.Close()
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
	return f.Close()
}

// Written returns all entries this machine archived, with their tapes.
func (c *Catalog) Written() ([]Hit, error) {
	names, err := filepath.Glob(filepath.Join(c.Dir, "written", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for _, n := range names {
		id := strings.TrimSuffix(filepath.Base(n), ".jsonl")
		if !manifest.ValidID(id) {
			continue
		}
		t, err := c.Tape(id)
		if err != nil {
			return nil, err
		}
		if t == nil {
			t = &Tape{Volume: manifest.Volume{ID: id}}
		}
		f, err := os.Open(n)
		if err != nil {
			return nil, err
		}
		// Damaged lines are skipped: that only makes purge keep more files
		// and put skip fewer.
		entries, _, err := manifest.ReadJSONLLenient[manifest.Entry](f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		for _, e := range entries {
			hits = append(hits, Hit{Tape: *t, Entry: e})
		}
	}
	return hits, nil
}

// ParityDir returns the directory for staged parity of tape id.
func (c *Catalog) ParityDir(id string) string { return filepath.Join(c.Dir, "parity", id) }

// Continuation records an archive run that filled one or more tapes, so
// running the same command on the next tape skips what is already on them.
type Continuation struct {
	Source  string    `json:"source"`
	Prefix  string    `json:"prefix"`
	Tapes   []string  `json:"tapes"`
	Updated time.Time `json:"updated"`
}

func (c *Catalog) continuationPath(source, prefix string) string {
	h := sha256.Sum256([]byte(source + "\x00" + prefix))
	return filepath.Join(c.Dir, "continue", hex.EncodeToString(h[:16])+".json")
}

// Continuation returns the open continuation for source and prefix, or nil.
func (c *Catalog) Continuation(source, prefix string) (*Continuation, error) {
	b, err := os.ReadFile(c.continuationPath(source, prefix))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cont Continuation
	if err := json.Unmarshal(b, &cont); err != nil || cont.Source != source || cont.Prefix != prefix {
		return nil, nil
	}
	ids := cont.Tapes[:0]
	for _, id := range cont.Tapes {
		if manifest.ValidID(id) {
			ids = append(ids, id)
		}
	}
	cont.Tapes = ids
	return &cont, nil
}

// AddContinuation notes that the run for source and prefix filled tape id.
func (c *Catalog) AddContinuation(source, prefix, id string) error {
	cont, err := c.Continuation(source, prefix)
	if err != nil {
		return err
	}
	if cont == nil {
		cont = &Continuation{Source: source, Prefix: prefix}
	}
	if !slices.Contains(cont.Tapes, id) {
		cont.Tapes = append(cont.Tapes, id)
	}
	cont.Updated = time.Now().UTC()
	b, err := json.MarshalIndent(cont, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(c.Dir, "continue"), 0o755); err != nil {
		return err
	}
	return manifest.WriteFileAtomic(c.continuationPath(source, prefix), append(b, '\n'))
}

// ClearContinuation ends the continuation for source and prefix.
func (c *Catalog) ClearContinuation(source, prefix string) error {
	err := os.Remove(c.continuationPath(source, prefix))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// FindTape identifies a cataloged tape whose volume record cannot be read:
// by its LTFS volume UUID if known, otherwise by its manifest entries. It
// returns "" unless exactly one tape matches.
func (c *Catalog) FindTape(ltfsUUID string, entries []manifest.Entry) string {
	tapes, err := c.Tapes()
	if err != nil {
		return ""
	}
	var match []string
	for _, t := range tapes {
		if ltfsUUID != "" {
			if t.LTFSUUID == ltfsUUID {
				match = append(match, t.ID)
			}
			continue
		}
		if len(entries) == 0 {
			continue
		}
		known, err := c.Entries(t.ID)
		if err != nil {
			continue
		}
		sums := make(map[string]string, len(known))
		for _, e := range known {
			sums[e.Path] = e.SHA256
		}
		all := true
		for _, e := range entries[:min(len(entries), 50)] {
			if sums[e.Path] != e.SHA256 {
				all = false
				break
			}
		}
		if all {
			match = append(match, t.ID)
		}
	}
	if len(match) == 1 {
		return match[0]
	}
	return ""
}

// JournalDir returns the directory for resume journals of tape id.
func (c *Catalog) JournalDir(id string) string { return filepath.Join(c.Dir, "journal", id) }

// Import copies the manifest of the tape mounted at tapeRoot into the
// catalog. Local verification history is kept.
func (c *Catalog) Import(tapeRoot string) (*Tape, error) {
	tape, err := manifest.Open(tapeRoot)
	if err != nil {
		return nil, err
	}
	defer tape.Close()
	vol, err := tape.Volume()
	if err != nil {
		return nil, err
	}
	if vol == nil {
		return nil, errors.New("tape has no tapemgr volume record; nothing archived yet")
	}
	entries, err := tape.Entries()
	if err != nil {
		return nil, err
	}
	data, err := manifest.MarshalJSONL(entries)
	if err != nil {
		return nil, err
	}

	t, err := c.Tape(vol.ID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		t = &Tape{}
	}
	t.Volume = *vol
	t.Files, t.Bytes = 0, 0
	for _, e := range entries {
		t.Files++
		if e.Ref == nil {
			t.Bytes += e.Size
		}
	}
	t.ImportedAt = time.Now().UTC()

	if err := manifest.WriteFileAtomic(c.manifestPath(vol.ID), data); err != nil {
		return nil, err
	}
	return t, c.save(t)
}

// RecordVerify appends a verification result to the tape record.
func (c *Catalog) RecordVerify(id string, v Verification) error {
	t, err := c.Tape(id)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("tape %s is not in the catalog", id)
	}
	t.Verifications = append(t.Verifications, v)
	return c.save(t)
}

func (c *Catalog) save(t *Tape) error {
	if !manifest.ValidID(t.ID) {
		return fmt.Errorf("invalid tape ID %q", t.ID)
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return manifest.WriteFileAtomic(c.recordPath(t.ID), append(b, '\n'))
}

// Tape returns the record for id, or nil if unknown.
func (c *Catalog) Tape(id string) (*Tape, error) {
	if !manifest.ValidID(id) {
		return nil, fmt.Errorf("invalid tape ID %q", id)
	}
	b, err := os.ReadFile(c.recordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Tape
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", c.recordPath(id), err)
	}
	return &t, nil
}

// Tapes returns all tape records ordered by label, then ID.
func (c *Catalog) Tapes() ([]Tape, error) {
	names, err := filepath.Glob(filepath.Join(c.Dir, "tapes", "*.json"))
	if err != nil {
		return nil, err
	}
	var tapes []Tape
	for _, n := range names {
		id := strings.TrimSuffix(filepath.Base(n), ".json")
		if !manifest.ValidID(id) {
			continue
		}
		t, err := c.Tape(id)
		if err != nil {
			return nil, err
		}
		if t != nil {
			tapes = append(tapes, *t)
		}
	}
	sort.Slice(tapes, func(i, j int) bool {
		if tapes[i].Label != tapes[j].Label {
			return tapes[i].Label < tapes[j].Label
		}
		return tapes[i].ID < tapes[j].ID
	})
	return tapes, nil
}

// Entries returns the cataloged manifest of tape id.
func (c *Catalog) Entries(id string) ([]manifest.Entry, error) {
	f, err := os.Open(c.manifestPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return manifest.Read(f)
}

// Search returns entries whose path contains q (case-insensitive) or whose
// SHA-256 starts with q.
func (c *Catalog) Search(q string) ([]Hit, error) {
	q = strings.ToLower(q)
	var hits []Hit
	err := c.each(func(t Tape, e manifest.Entry) {
		if strings.Contains(strings.ToLower(e.Path), q) || strings.HasPrefix(e.SHA256, q) {
			hits = append(hits, Hit{Tape: t, Entry: e})
		}
	})
	return hits, err
}

// BySize returns all entries with real content (no references), keyed by
// size. It is the candidate index for deduplication.
func (c *Catalog) BySize() (map[int64][]Hit, error) {
	m := make(map[int64][]Hit)
	err := c.each(func(t Tape, e manifest.Entry) {
		if e.Ref == nil {
			m[e.Size] = append(m[e.Size], Hit{Tape: t, Entry: e})
		}
	})
	return m, err
}

// All returns every cataloged entry with its tape.
func (c *Catalog) All() ([]Hit, error) {
	var hits []Hit
	err := c.each(func(t Tape, e manifest.Entry) {
		hits = append(hits, Hit{Tape: t, Entry: e})
	})
	return hits, err
}

func (c *Catalog) each(fn func(Tape, manifest.Entry)) error {
	tapes, err := c.Tapes()
	if err != nil {
		return err
	}
	for _, t := range tapes {
		entries, err := c.Entries(t.ID)
		if err != nil {
			return err
		}
		for _, e := range entries {
			fn(t, e)
		}
	}
	return nil
}
