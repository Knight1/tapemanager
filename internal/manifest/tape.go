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
	manifestSuffix   = ".manifest.jsonl"
	chunksSuffix     = ".chunks.jsonl"
	paritySuffix     = ".parity"
	parityListSuffix = ".parity.jsonl"
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

// Entries reads all manifest entries in write order. Any damage is an
// error: writing to a tape whose manifest cannot be fully read is unsafe.
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
		for i := range list {
			list[i].Segment = n
		}
		all = append(all, list...)
	}
	return all, nil
}

// EntriesLenient reads all manifest entries it can, for reading files back.
// Damaged lines or segments are skipped and reported in the returned
// problems, so one bad spot does not block restoring everything else.
func (t *Tape) EntriesLenient() ([]Entry, []string, error) {
	nums, err := t.segments()
	if err != nil {
		return nil, nil, err
	}
	var all []Entry
	var problems []string
	for _, n := range nums {
		list, p := readLenient[Entry](t.root, segmentName(n, manifestSuffix))
		for i := range list {
			list[i].Segment = n
		}
		all = append(all, list...)
		problems = append(problems, p...)
	}
	return all, problems, nil
}

// Chunks reads all chunk records it can, keyed by path. Chunk hashes are
// optional, so damage is reported in problems rather than as an error.
func (t *Tape) Chunks() (map[string]Chunks, []string, error) {
	nums, err := t.segments()
	if err != nil {
		return nil, nil, err
	}
	m := make(map[string]Chunks)
	var problems []string
	for _, n := range nums {
		list, p := readLenient[Chunks](t.root, segmentName(n, chunksSuffix))
		problems = append(problems, p...)
		for _, c := range list {
			m[c.Path] = c
		}
	}
	return m, problems, nil
}

// Parity reads all parity records it can, keyed by path. Damage only costs
// the parity of the affected files and is reported in problems.
func (t *Tape) Parity() (map[string]Parity, []string, error) {
	nums, err := t.segments()
	if err != nil {
		return nil, nil, err
	}
	m := make(map[string]Parity)
	var problems []string
	for _, n := range nums {
		list, p := readLenient[Parity](t.root, segmentName(n, parityListSuffix))
		problems = append(problems, p...)
		for _, par := range list {
			par.Segment = n
			m[par.Path] = par
		}
	}
	return m, problems, nil
}

// ParitySegments returns the segments that have parity, with the parity
// records of each in offset order.
func (t *Tape) ParitySegments() (map[int][]Parity, []string, error) {
	par, problems, err := t.Parity()
	if err != nil {
		return nil, nil, err
	}
	segs := map[int][]Parity{}
	for _, p := range par {
		segs[p.Segment] = append(segs[p.Segment], p)
	}
	for _, list := range segs {
		sort.Slice(list, func(i, j int) bool { return list[i].Offset < list[j].Offset })
	}
	return segs, problems, nil
}

// OpenParity opens the parity data file of segment n.
func (t *Tape) OpenParity(n int) (*os.File, error) {
	return t.root.Open(segmentName(n, paritySuffix))
}

func readLenient[T Validator](root *os.Root, name string) ([]T, []string) {
	f, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", name, err)}
	}
	defer f.Close()
	list, bad, err := ReadJSONLLenient[T](f)
	var problems []string
	if bad > 0 {
		problems = append(problems, fmt.Sprintf("%s: %d damaged records skipped", name, bad))
	}
	if err != nil {
		problems = append(problems, fmt.Sprintf("%s: %v", name, err))
	}
	return list, problems
}

// ReadParityShard reads parity shard i of the file described by p.
func (t *Tape) ReadParityShard(p Parity, i int64) ([]byte, error) {
	if i < 0 || i >= p.Layout.ParityShards() {
		return nil, fmt.Errorf("parity shard %d out of range", i)
	}
	f, err := t.root.Open(segmentName(p.Segment, paritySuffix))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, p.Layout.ShardSize)
	if _, err := f.ReadAt(b, p.Offset+i*p.Layout.ShardSize); err != nil {
		return nil, err
	}
	return b, nil
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

// Segment is one batch of records to write to tape.
type Segment struct {
	Entries []Entry
	Chunks  []Chunks
	// Parity records, with offsets into ParityData.
	Parity     []Parity
	ParityData io.Reader
}

// ParityError reports that a segment was written but its parity was not.
// The archived files are fully recorded; only their parity is missing.
type ParityError struct{ Err error }

func (e *ParityError) Error() string { return "writing parity: " + e.Err.Error() }
func (e *ParityError) Unwrap() error { return e.Err }

// WriteSegment records a batch as a new segment, and rewrites SHA256SUMS to
// cover all entries. all must contain every entry on the tape including the
// new ones.
//
// Each file is written in one piece so it stays contiguous on tape. The
// manifest file is what makes the segment count, so it is written before
// the large and optional parity data: a full tape or a failed parity write
// then costs only parity, never the record of what was archived. A parity
// failure is returned as *ParityError after the segment is committed.
func (t *Tape) WriteSegment(seg Segment, all []Entry) error {
	if len(seg.Entries) == 0 {
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
	// Leftovers of an aborted attempt at this segment number must not be
	// adopted by the new segment.
	for _, suffix := range []string{chunksSuffix, paritySuffix, parityListSuffix} {
		for _, name := range []string{segmentName(next, suffix), segmentName(next, suffix) + ".tmp"} {
			if err := t.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}

	c, err := MarshalJSONL(seg.Chunks)
	if err != nil {
		return err
	}
	if err := t.writeAtomic(segmentName(next, chunksSuffix), c); err != nil {
		return err
	}
	m, err := MarshalJSONL(seg.Entries)
	if err != nil {
		return err
	}
	if err := t.writeAtomic(segmentName(next, manifestSuffix), m); err != nil {
		return err
	}
	if err := t.writeSums(all); err != nil {
		return err
	}

	if len(seg.Parity) == 0 {
		return nil
	}
	// The list is written after the data, so a list only exists for
	// complete parity data.
	if err := t.writeStream(segmentName(next, paritySuffix), seg.ParityData); err != nil {
		t.root.Remove(segmentName(next, paritySuffix) + ".tmp")
		return &ParityError{err}
	}
	p, err := MarshalJSONL(seg.Parity)
	if err == nil {
		err = t.writeAtomic(segmentName(next, parityListSuffix), p)
	}
	if err != nil {
		return &ParityError{err}
	}
	return nil
}

// WriteParity adds parity to existing segment n that has none, for example
// after a crash interrupted the parity write following its manifest. It
// refuses if the segment already has a parity list.
func (t *Tape) WriteParity(n int, pars []Parity, data io.Reader) error {
	if len(pars) == 0 {
		return nil
	}
	list := segmentName(n, parityListSuffix)
	if _, err := t.root.Lstat(list); err == nil {
		return fmt.Errorf("segment %d already has parity", n)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := t.root.Lstat(segmentName(n, manifestSuffix)); err != nil {
		return fmt.Errorf("segment %d: %w", n, err)
	}
	for _, name := range []string{segmentName(n, paritySuffix), segmentName(n, paritySuffix) + ".tmp", list + ".tmp"} {
		if err := t.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := t.writeStream(segmentName(n, paritySuffix), data); err != nil {
		t.root.Remove(segmentName(n, paritySuffix) + ".tmp")
		return err
	}
	p, err := MarshalJSONL(pars)
	if err != nil {
		return err
	}
	return t.writeAtomic(list, p)
}

// EnsureSums rewrites SHA256SUMS if it does not match all, for example after
// a crash between a segment's manifest and its SHA256SUMS update.
func (t *Tape) EnsureSums(all []Entry) error {
	want := sumsContent(all)
	f, err := t.root.Open(SumsName)
	if err == nil {
		got, rerr := io.ReadAll(io.LimitReader(f, int64(len(want))+1))
		f.Close()
		if rerr == nil && string(got) == want {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(all) == 0 {
		return nil
	}
	return t.writeAtomic(SumsName, []byte(want))
}

func (t *Tape) writeStream(name string, r io.Reader) error {
	tmp := name + ".tmp"
	f, err := t.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(f, struct{ io.Reader }{r}, make([]byte, 4<<20)); err != nil {
		f.Close()
		return err
	}
	return finishAtomic(f, nil, func() error { return t.renameSynced(tmp, name) })
}

func (t *Tape) writeSums(all []Entry) error {
	return t.writeAtomic(SumsName, []byte(sumsContent(all)))
}

func sumsContent(all []Entry) string {
	var b strings.Builder
	for _, e := range all {
		if e.Ref == nil {
			// Encrypted files are listed as stored, so sha256sum -c
			// checks them without the key.
			s := e.Stored()
			fmt.Fprintf(&b, "%s  %s\n", s.SHA256, s.Path)
		}
	}
	return b.String()
}

// writeAtomic replaces name on tape with data through a synced temporary
// file and an atomic rename, then syncs the directory. See WriteFileAtomic.
func (t *Tape) writeAtomic(name string, data []byte) error {
	tmp := name + ".tmp"
	f, err := t.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return finishAtomic(f, data, func() error { return t.renameSynced(tmp, name) })
}

func (t *Tape) renameSynced(tmp, name string) error {
	if err := t.root.Rename(tmp, name); err != nil {
		return err
	}
	return t.SyncDir(path.Dir(name))
}

// SyncDir syncs directory dir on tape.
func (t *Tape) SyncDir(dir string) error {
	d, err := t.root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return ignoreUnsupported(d.Sync())
}
