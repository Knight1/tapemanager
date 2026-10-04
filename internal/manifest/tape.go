package manifest

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	manifestSuffix = ".manifest.jsonl"
	chunksSuffix   = ".chunks.jsonl"
)

// Tape gives access to the metadata on a mounted tape. All file access goes
// through an os.Root, so a crafted tape cannot redirect reads or writes
// outside its mount point with symlinks.
type Tape struct {
	root *os.Root
}

// Open opens the tape mounted at dir.
func Open(dir string) (*Tape, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Tape{root: root}, nil
}

// Root returns the tape's os.Root for data file access.
func (t *Tape) Root() *os.Root { return t.root }

// Close releases the tape.
func (t *Tape) Close() error { return t.root.Close() }

// Volume reads the tape identity. A missing file yields nil.
func (t *Tape) Volume() (*Volume, error) {
	f, err := t.root.Open(path.Join(Dir, VolumeName))
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
func (t *Tape) InitVolume(label, ltfsUUID string) (*Volume, error) {
	v, err := t.Volume()
	if err != nil || v != nil {
		return v, err
	}
	v = &Volume{ID: newID(), Label: label, LTFSUUID: ltfsUUID, Created: time.Now().UTC()}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := t.root.MkdirAll(Dir, 0o755); err != nil {
		return nil, err
	}
	if err := t.writeAtomic(path.Join(Dir, VolumeName), append(b, '\n')); err != nil {
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

// segments returns the numbers of all complete segments in order. A segment
// is complete once its manifest file exists.
func (t *Tape) segments() ([]int, error) {
	d, err := t.root.Open(path.Join(Dir, SegmentsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	var nums []int
	for _, n := range names {
		num, ok := strings.CutSuffix(n, manifestSuffix)
		if !ok || len(num) != 6 || strings.Trim(num, "0123456789") != "" {
			continue
		}
		i, err := strconv.Atoi(num)
		if err != nil || i < 1 {
			continue
		}
		nums = append(nums, i)
	}
	sort.Ints(nums)
	return nums, nil
}

func segmentName(n int, suffix string) string {
	return path.Join(Dir, SegmentsDir, fmt.Sprintf("%06d%s", n, suffix))
}

// Entries reads all manifest entries in write order.
func (t *Tape) Entries() ([]Entry, error) {
	nums, err := t.segments()
	if err != nil {
		return nil, err
	}
	var all []Entry
	for _, n := range nums {
		list, err := readFile[Entry](t.root, segmentName(n, manifestSuffix))
		if err != nil {
			return nil, err
		}
		all = append(all, list...)
	}
	return all, nil
}

// Chunks reads all chunk records, keyed by path.
func (t *Tape) Chunks() (map[string]Chunks, error) {
	nums, err := t.segments()
	if err != nil {
		return nil, err
	}
	m := make(map[string]Chunks)
	for _, n := range nums {
		list, err := readFile[Chunks](t.root, segmentName(n, chunksSuffix))
		if err != nil {
			return nil, err
		}
		for _, c := range list {
			m[c.Path] = c
		}
	}
	return m, nil
}

func readFile[T Validator](root *os.Root, name string) ([]T, error) {
	f, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	list, err := ReadJSONL[T](f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return list, nil
}

// WriteSegment records entries and their chunk hashes as a new segment, and
// rewrites SHA256SUMS to cover all entries. all must contain every entry on
// the tape including the new ones.
//
// Each file is written in one piece so it stays contiguous on tape. The
// chunk file comes first and the manifest file last, because the manifest
// file is what makes the segment count.
func (t *Tape) WriteSegment(entries []Entry, chunks []Chunks, all []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	nums, err := t.segments()
	if err != nil {
		return err
	}
	next := 1
	if len(nums) > 0 {
		next = nums[len(nums)-1] + 1
	}
	if next > 999999 {
		return errors.New("too many manifest segments")
	}
	if err := t.root.MkdirAll(path.Join(Dir, SegmentsDir), 0o755); err != nil {
		return err
	}
	c, err := MarshalJSONL(chunks)
	if err != nil {
		return err
	}
	if err := t.writeAtomic(segmentName(next, chunksSuffix), c); err != nil {
		return err
	}
	m, err := MarshalJSONL(entries)
	if err != nil {
		return err
	}
	if err := t.writeAtomic(segmentName(next, manifestSuffix), m); err != nil {
		return err
	}
	return t.writeSums(all)
}

func (t *Tape) writeSums(all []Entry) error {
	var b strings.Builder
	for _, e := range all {
		if e.Ref == nil {
			fmt.Fprintf(&b, "%s  %s\n", e.SHA256, e.Path)
		}
	}
	return t.writeAtomic(SumsName, []byte(b.String()))
}

func (t *Tape) writeAtomic(name string, data []byte) error {
	tmp := name + ".tmp"
	f, err := t.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return finishAtomic(f, data, func() error { return t.root.Rename(tmp, name) })
}
