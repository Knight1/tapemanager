// Package archive implements the archive workflow: streaming files onto a
// mounted tape while hashing them, and verifying them by reading them back.
package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/ltfs"
	"github.com/Knight1/tapemanager/internal/manifest"
	"github.com/Knight1/tapemanager/internal/parity"
)

// PartialSuffix marks a file on tape whose transfer has not completed.
const PartialSuffix = ".tapemgr-partial"

const (
	// DefaultChunkSize is the read/write and chunk hash unit. Large
	// sequential writes keep the drive streaming instead of shoe-shining.
	DefaultChunkSize = 4 << 20
	// DefaultCheckpointEvery is how much data is written between resume
	// checkpoints. Each checkpoint syncs the file to tape.
	DefaultCheckpointEvery = 1 << 30
	// MaxCopies bounds PutOptions.Copies and PurgeOptions.Copies.
	MaxCopies = 9
	// DefaultDedupMinSize is the smallest file considered for deduplication.
	DefaultDedupMinSize = 1 << 20
	// DefaultFlushEvery is how much archived data accumulates before its
	// manifest records are written to the tape as one segment. Larger
	// values mean fewer, bigger metadata files on tape.
	DefaultFlushEvery = 100 << 30
)

// ErrTapeFull is returned when the next file does not fit on the tape. Files
// archived until then are fully recorded.
var ErrTapeFull = errors.New("tape is full")

// freeSpace reports the bytes available at a path. Tests replace it.
var (
	freeSpace     = ltfs.FreeSpace
	realFreeSpace = ltfs.FreeSpace
)

// syncIndex forces the LTFS index to tape. Tests replace it.
var (
	syncIndex     = ltfs.SyncIndex
	realSyncIndex = ltfs.SyncIndex
)

// readOnly reports a read-only mount. Tests replace it.
var readOnly = ltfs.ReadOnly

// metadataReserve estimates the tape space the next segment write needs
// beyond staged parity: manifest and chunk lists, and a rewritten
// SHA256SUMS, plus a fixed margin.
func metadataReserve(entries int, bytes, chunkSize int64) int64 {
	return 256<<20 + int64(entries)*400 + bytes/chunkSize*100
}

// testHook lets tests simulate interruptions at named stages.
var testHook func(stage string, offset int64) error

func hook(stage string, offset int64) error {
	if testHook == nil {
		return nil
	}
	return testHook(stage, offset)
}

// PutOptions configures Put.
type PutOptions struct {
	TapeRoot string           // LTFS mount point
	Source   string           // file or directory to archive
	Prefix   string           // destination directory on tape; defaults to the source base name
	Label    string           // tape label, used only when the tape is first initialized
	Catalog  *catalog.Catalog // local catalog, required
	Dedup    bool             // store a reference instead of content already on tape
	Parity   int              // parity overhead in percent, 0 for none
	// Copies is how many different tapes each file should be on, 1 by
	// default. Files that already have that many copies on other tapes are
	// skipped, so running the same put on the next tape after a full one
	// continues where it stopped, and Copies 2 makes a second copy.
	Copies int
	// Again archives files even if they already have enough copies.
	Again    bool
	Log      io.Writer // per-file output
	Progress io.Writer // progress bar output, nil to disable

	ChunkSize       int64 // default DefaultChunkSize
	CheckpointEvery int64 // default DefaultCheckpointEvery
	DedupMinSize    int64 // default DefaultDedupMinSize
	FlushEvery      int64 // default DefaultFlushEvery
}

// PutSummary reports what Put did.
type PutSummary struct {
	Tape      *manifest.Volume
	Files     int
	Skipped   int // already on this tape
	Elsewhere int // already on enough other tapes
	Deduped   int
	Bytes     int64
	Resumed   int64 // bytes not rewritten thanks to a resume checkpoint
	Duration  time.Duration
}

type job struct {
	src  string
	rel  string // slash separated, relative to the tape root
	info fs.FileInfo
}

// Put archives opts.Source onto the tape. Files are written strictly one
// after another in lexical order. Each file's SHA-256 is computed from the
// same bytes that are written. Its manifest record goes to the local
// pending log once the file is synced and renamed into place, and pending
// records are written to the tape in large segments (see package manifest).
//
// Rerunning an interrupted Put skips files already in the manifest and
// continues a partially written file from its last checkpoint.
func Put(opts PutOptions) (sum PutSummary, err error) {
	start := time.Now()
	if opts.Catalog == nil {
		return sum, errors.New("catalog is required")
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = DefaultChunkSize
	}
	if opts.CheckpointEvery <= 0 {
		opts.CheckpointEvery = DefaultCheckpointEvery
	}
	if opts.DedupMinSize <= 0 {
		opts.DedupMinSize = DefaultDedupMinSize
	}
	if opts.FlushEvery <= 0 {
		opts.FlushEvery = DefaultFlushEvery
	}
	if opts.Copies <= 0 {
		opts.Copies = 1
	}
	if opts.Copies > MaxCopies {
		return sum, fmt.Errorf("at most %d copies are supported", MaxCopies)
	}
	m, err := parity.Shards(opts.Parity)
	if err != nil {
		return sum, err
	}
	if opts.ChunkSize < manifest.MinChunkSize || opts.ChunkSize > manifest.MaxChunkSize {
		return sum, fmt.Errorf("chunk size must be between %d and %d bytes", manifest.MinChunkSize, manifest.MaxChunkSize)
	}
	if m > 0 && opts.ChunkSize > parity.MaxShardSize {
		return sum, fmt.Errorf("parity needs a chunk size of at most %d bytes", parity.MaxShardSize)
	}

	src, err := filepath.Abs(opts.Source)
	if err != nil {
		return sum, err
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = filepath.Base(src)
	}
	prefix = path.Clean(filepath.ToSlash(prefix))
	if !manifest.ValidPath(prefix) {
		return sum, fmt.Errorf("invalid destination prefix %q", prefix)
	}

	if err := checkWritable(opts.TapeRoot); err != nil {
		return sum, err
	}
	jobs, err := plan(src, prefix, opts.Log)
	if err != nil {
		return sum, err
	}

	tape, err := manifest.Open(opts.TapeRoot)
	if err != nil {
		return sum, err
	}
	defer tape.Close()
	vol, err := tape.InitVolume(opts.Label, ltfs.VolumeUUID(opts.TapeRoot))
	if err != nil {
		return sum, err
	}
	sum.Tape = vol

	existing, err := tape.Entries()
	if err != nil {
		return sum, err
	}
	parityDir := opts.Catalog.ParityDir(vol.ID)
	journalDir := opts.Catalog.JournalDir(vol.ID)
	pend, err := openPending(opts.Catalog.PendingPath(vol.ID))
	if err != nil {
		return sum, err
	}
	defer pend.close()

	// flush writes the pending records to the tape as one new segment.
	flush := func() error {
		if len(pend.records) == 0 {
			return nil
		}
		if err := hook("flush", int64(len(pend.records))); err != nil {
			return err
		}
		onTape := make(map[string]manifest.Entry, len(existing))
		for _, e := range existing {
			onTape[e.Path] = e
		}
		// Parity of records already on tape whose segment lost its parity
		// write to a crash; written as an addendum to that segment.
		tapeParity, _, err := tape.Parity()
		if err != nil {
			return err
		}
		addendum := map[int][]pendingRecord{}
		var entries, written []manifest.Entry
		var chunks []manifest.Chunks
		var pars []manifest.Parity
		var staged []string
		var parityData []io.Reader
		var parityOffset int64
		defer func() {
			for _, r := range parityData {
				r.(*os.File).Close()
			}
		}()
		for _, r := range pend.records {
			// A crash after the segment was written but before the
			// pending log was cleared leaves records already on tape.
			if e, ok := onTape[r.Entry.Path]; ok {
				written = append(written, r.Entry)
				if e.SHA256 != r.Entry.SHA256 {
					return fmt.Errorf("pending record for %s conflicts with the tape manifest", r.Entry.Path)
				}
				if _, has := tapeParity[r.Entry.Path]; r.Parity != nil && !has {
					addendum[e.Segment] = append(addendum[e.Segment], r)
				}
				continue
			}
			// After a host crash LTFS rolls back to its last index, which
			// can undo a file that was complete when it was recorded here.
			// Only files really on tape are recorded; the others are
			// archived again on the next run.
			if r.Entry.Ref == nil {
				st, err := tape.Root().Lstat(filepath.FromSlash(r.Entry.Path))
				if err != nil || !st.Mode().IsRegular() || st.Size() != r.Entry.Size {
					fmt.Fprintf(opts.Log, "WARNING:   %s is no longer complete on tape (interrupted run); it will be archived again\n", r.Entry.Path)
					continue
				}
			}
			written = append(written, r.Entry)
			entries = append(entries, r.Entry)
			if r.Chunks != nil {
				chunks = append(chunks, *r.Chunks)
			}
			if r.Parity != nil {
				ppath := filepath.Join(parityDir, fileKey(r.Entry.Path)+".bin")
				if !stagedParityOK(ppath, r.Parity) {
					fmt.Fprintf(opts.Log, "WARNING:   staged parity for %s is missing or damaged; it is archived without parity\n", r.Entry.Path)
					continue
				}
				f, err := os.Open(ppath)
				if err != nil {
					return err
				}
				parityData = append(parityData, f)
				p := *r.Parity
				p.Offset = parityOffset
				parityOffset += p.Layout.ParitySize()
				pars = append(pars, p)
				staged = append(staged, ppath)
			}
		}
		all := append(existing[:len(existing):len(existing)], entries...)
		seg := manifest.Segment{Entries: entries, Chunks: chunks, Parity: pars, ParityData: hookReader{io.MultiReader(parityData...)}}
		if err := tape.WriteSegment(seg, all); err != nil {
			var pe *manifest.ParityError
			if !errors.As(err, &pe) {
				return fmt.Errorf("writing manifest segment: %w", err)
			}
			// The files are recorded; only their parity is missing.
			fmt.Fprintf(opts.Log, "WARNING:   %v; %d files in this batch have no parity\n", err, len(pars))
		}
		existing = all
		for n, recs := range addendum {
			paths, err := writeParityAddendum(tape, n, recs, parityDir)
			if err != nil {
				fmt.Fprintf(opts.Log, "WARNING:   could not add parity to segment %d: %v\n", n, err)
				continue
			}
			staged = append(staged, paths...)
		}
		// The local pending log is the only other copy of these records.
		// Make LTFS write its index first, so a host crash cannot roll the
		// tape back to before this segment once the log is cleared.
		if err := syncIndex(opts.TapeRoot); err != nil {
			return err
		}
		// Recorded after the tape write and before clearing, so a crash in
		// between repeats it rather than losing it. Duplicates are harmless.
		if err := opts.Catalog.RecordWritten(vol.ID, written); err != nil {
			return fmt.Errorf("recording written files: %w", err)
		}
		if err := pend.clear(); err != nil {
			return err
		}
		for _, p := range staged {
			os.Remove(p)
		}
		return nil
	}

	if n := len(pend.records); n > 0 {
		fmt.Fprintf(opts.Log, "RECOVERING: writing %d records from an interrupted run to the tape manifest\n", n)
		if err := flush(); err != nil {
			return sum, err
		}
	}

	removeStaleParity(parityDir, journalDir)
	if err := tape.EnsureSums(existing); err != nil {
		return sum, err
	}

	done := make(map[string]manifest.Entry, len(existing))
	for _, e := range existing {
		done[e.Path] = e
	}

	// Copies on other tapes come from the catalog. The catalog's view of
	// this tape may be stale, so copies here are tracked separately.
	others, err := opts.Catalog.Copies()
	if err != nil {
		return sum, err
	}
	here := map[string]bool{}
	for _, e := range existing {
		if e.Ref == nil {
			here[e.SHA256] = true
		}
	}
	copiesOf := func(sha string) (n int, tapes []catalog.Tape) {
		tapes = others.Tapes(sha, vol.ID)
		n = len(tapes)
		if here[sha] {
			n++
		}
		return n, tapes
	}
	// Where a source file went before is taken from the records this
	// machine wrote, matched by path, size and mtime, which gives its
	// SHA-256 without reading it again.
	writtenBefore := map[string][]catalog.Hit{}
	if !opts.Again {
		written, err := opts.Catalog.Written()
		if err != nil {
			return sum, err
		}
		for _, h := range written {
			if h.Tape.ID != vol.ID {
				writtenBefore[h.Entry.Source] = append(writtenBefore[h.Entry.Source], h)
			}
		}
	}

	var dedup map[int64][]catalog.Hit
	if opts.Dedup {
		if dedup, err = opts.Catalog.BySize(); err != nil {
			return sum, err
		}
		for _, e := range existing {
			if e.Ref == nil {
				dedup[e.Size] = append(dedup[e.Size], catalog.Hit{Tape: catalog.Tape{Volume: *vol}, Entry: e})
			}
		}
	}

	defer func() {
		// Records of completed files are valid even after a failure.
		if ferr := flush(); ferr != nil {
			err = errors.Join(err, ferr)
		}
		// Keep the catalog in step with the tape.
		if _, cerr := opts.Catalog.Import(opts.TapeRoot); cerr != nil {
			err = errors.Join(err, fmt.Errorf("updating catalog (tape data is fine, run 'tapemgr catalog import'): %w", cerr))
		}
		sum.Duration = time.Since(start)
	}()

	record := func(r pendingRecord) error {
		if err := pend.add(r); err != nil {
			return fmt.Errorf("writing pending log: %w", err)
		}
		done[r.Entry.Path] = r.Entry
		if pend.bytes >= opts.FlushEvery {
			return flush()
		}
		return nil
	}

	for _, j := range jobs {
		if e, ok := done[j.rel]; ok {
			if e.Size == j.info.Size() && e.MTime.Equal(j.info.ModTime()) {
				fmt.Fprintf(opts.Log, "SKIPPING:  %s (already archived)\n", j.src)
				sum.Skipped++
				continue
			}
			return sum, fmt.Errorf("%s: a different version is already archived as %s", j.src, j.rel)
		}
		if h := archivedElsewhere(j, writtenBefore[j.src]); h != nil {
			if n, tapes := copiesOf(h.Entry.SHA256); n >= opts.Copies {
				fmt.Fprintf(opts.Log, "SKIPPING:  %s (already on %s)\n", j.src, tapeList(tapes))
				sum.Elsewhere++
				continue
			}
		}

		if opts.Dedup && j.info.Size() >= opts.DedupMinSize && len(dedup[j.info.Size()]) > 0 {
			// A reference to another tape is no extra copy: it is only
			// used when that content already has enough copies.
			var candidates []catalog.Hit
			for _, c := range dedup[j.info.Size()] {
				if c.Tape.ID == vol.ID {
					candidates = append(candidates, c)
				} else if n, _ := copiesOf(c.Entry.SHA256); n >= opts.Copies && c.Tape.Retired == nil {
					candidates = append(candidates, c)
				}
			}
			e, err := findDuplicate(j, candidates)
			if err != nil {
				return sum, fmt.Errorf("%s: %w", j.src, err)
			}
			if e != nil {
				if err := record(pendingRecord{Entry: *e}); err != nil {
					return sum, err
				}
				fmt.Fprintf(opts.Log, "DUPLICATE: %s\n       same as %s on tape %s\n\n", j.src, e.Ref.Path, e.Ref.Tape)
				sum.Deduped++
				continue
			}
		}

		layout := parity.ForFile(j.info.Size(), opts.ChunkSize, m)
		check := func(reused int64) error {
			return checkSpace(opts, j, layout, pend, len(existing), reused)
		}

		fmt.Fprintf(opts.Log, "ARCHIVING: %s\n       %s\n", j.src, FormatBytes(j.info.Size()))
		jpath := journalPath(journalDir, j.rel)
		res, err := archiveFile(fileState{
			opts:  opts,
			root:  tape.Root(),
			j:     j,
			jpath: jpath,
			ppath: filepath.Join(parityDir, fileKey(j.rel)+".bin"),
			m:     m,
			check: check,
		})
		if err != nil {
			return sum, fmt.Errorf("%s: %w", j.src, err)
		}
		e, resumed := res.entry, res.resumed
		if resumed > 0 {
			fmt.Fprintf(opts.Log, "       resumed at %s\n", FormatBytes(resumed))
		}
		if res.note != "" {
			fmt.Fprintf(opts.Log, "       NOTE: %s\n", res.note)
		}
		if err := hook("manifest", e.Size); err != nil {
			return sum, err
		}
		if err := record(pendingRecord{Entry: e, Chunks: res.chunks, Parity: res.parity}); err != nil {
			return sum, err
		}
		if err := os.Remove(jpath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return sum, err
		}
		fmt.Fprintf(opts.Log, "       SHA256: %s\n\n", e.SHA256)
		if opts.Dedup {
			dedup[e.Size] = append(dedup[e.Size], catalog.Hit{Tape: catalog.Tape{Volume: *vol}, Entry: e})
		}
		here[e.SHA256] = true
		sum.Files++
		sum.Bytes += e.Size
		sum.Resumed += resumed
	}
	return sum, nil
}

// syncTapeDir makes a rename in dir on tape durable as far as the
// filesystem allows. LTFS makes it durable with its next index write.
func syncTapeDir(root *os.Root, dir string) error {
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EOPNOTSUPP) {
		return err
	}
	return nil
}

// writeParityAddendum writes the staged parity of recs, which are already
// recorded in segment n, as that segment's parity. It returns the staged
// files that were written.
func writeParityAddendum(tape *manifest.Tape, n int, recs []pendingRecord, parityDir string) ([]string, error) {
	var pars []manifest.Parity
	var readers []io.Reader
	var paths []string
	defer func() {
		for _, r := range readers {
			r.(*os.File).Close()
		}
	}()
	var off int64
	for _, r := range recs {
		ppath := filepath.Join(parityDir, fileKey(r.Entry.Path)+".bin")
		if !stagedParityOK(ppath, r.Parity) {
			continue
		}
		f, err := os.Open(ppath)
		if err != nil {
			return nil, err
		}
		readers = append(readers, f)
		p := *r.Parity
		p.Offset = off
		off += p.Layout.ParitySize()
		pars = append(pars, p)
		paths = append(paths, ppath)
	}
	if err := tape.WriteParity(n, pars, io.MultiReader(readers...)); err != nil {
		return nil, err
	}
	return paths, nil
}

// hookReader lets tests make the parity data stream fail.
type hookReader struct{ r io.Reader }

func (h hookReader) Read(p []byte) (int, error) {
	if err := hook("parity-data", 0); err != nil {
		return 0, err
	}
	return h.r.Read(p)
}

// ChunkSizeOrDefault returns the chunk size Put will use.
func (o PutOptions) ChunkSizeOrDefault() int64 {
	if o.ChunkSize > 0 {
		return o.ChunkSize
	}
	return DefaultChunkSize
}

// archivedElsewhere returns a record of j on another tape with the same
// size and mtime, or nil.
func archivedElsewhere(j job, hits []catalog.Hit) *catalog.Hit {
	for i, h := range hits {
		if h.Entry.Size == j.info.Size() && h.Entry.MTime.Equal(j.info.ModTime()) {
			return &hits[i]
		}
	}
	return nil
}

// ErrReadOnly means the tape cannot be written.
var ErrReadOnly = errors.New("tape is read-only")

// checkWritable refuses a read-only tape before anything is scanned or
// written. LTFS mounts a write-protected cartridge read-only.
func checkWritable(root string) error {
	ro, err := readOnly(root)
	if err != nil {
		return err
	}
	if ro {
		return fmt.Errorf("%w: %s is mounted read-only (write-protected cartridge, or LTFS mounted it read-only); nothing was written", ErrReadOnly, root)
	}
	return nil
}

// tapeList names tapes for the log.
func tapeList(tapes []catalog.Tape) string {
	if len(tapes) == 0 {
		return "this tape"
	}
	names := make([]string, len(tapes))
	for i, t := range tapes {
		names[i] = tapeLabel(t)
	}
	if len(names) == 1 {
		return "tape " + names[0]
	}
	return "tapes " + strings.Join(names, ", ")
}

// checkSpace makes sure the next file fits. On tape it needs room for its
// data, its parity, the parity already staged for the next segment, and the
// segment's metadata, so the final segment write of a full tape still
// succeeds. On the catalog disk it needs room for its staged parity.
//
// reused is the part of the file already on tape from an interrupted run
// that is resumed. A partial file that is not resumed does not count: LTFS
// never reclaims space, so rewriting it needs the full size again.
func checkSpace(opts PutOptions, j job, l *parity.Layout, pend *pending, entries int, reused int64) error {
	var paritySize int64
	if l != nil {
		paritySize = l.ParitySize()
	}
	need := j.info.Size() - reused + paritySize + pend.parityBytes +
		metadataReserve(entries+len(pend.records)+1, pend.bytes+j.info.Size(), opts.ChunkSize)
	free, err := freeSpace(opts.TapeRoot)
	if err != nil {
		return err
	}
	if free < need {
		return fmt.Errorf("%w: %s needs %s, %s free; the remaining files were not archived. Insert a new tape and run the same command again", ErrTapeFull, j.src, FormatBytes(need), FormatBytes(free))
	}
	if paritySize > 0 {
		catFree, err := freeSpace(opts.Catalog.Dir)
		if err != nil {
			return err
		}
		if catFree < paritySize+64<<20 {
			return fmt.Errorf("catalog disk at %s is full: %s needs %s for staged parity, %s free", opts.Catalog.Dir, j.src, FormatBytes(paritySize), FormatBytes(catFree))
		}
	}
	return nil
}

// removeStaleParity deletes staged parity of files that are neither pending
// nor in progress. It runs after pending records were flushed, so only
// files with a resume journal still need their staged parity.
func removeStaleParity(parityDir, journalDir string) {
	names, _ := filepath.Glob(filepath.Join(parityDir, "*.bin"))
	for _, n := range names {
		key := strings.TrimSuffix(filepath.Base(n), ".bin")
		if _, err := os.Stat(filepath.Join(journalDir, key+".jsonl")); errors.Is(err, os.ErrNotExist) {
			os.Remove(n)
		}
	}
}

// findDuplicate hashes the source and returns a reference entry if a file
// with the same content is already archived.
func findDuplicate(j job, candidates []catalog.Hit) (*manifest.Entry, error) {
	f, err := openSource(j)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, struct{ io.Reader }{f}, make([]byte, DefaultChunkSize)); err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	for _, c := range candidates {
		if c.Entry.SHA256 == sum {
			return &manifest.Entry{
				Path:       j.rel,
				Size:       j.info.Size(),
				SHA256:     sum,
				MTime:      j.info.ModTime(),
				Source:     j.src,
				ArchivedAt: time.Now().UTC(),
				Ref:        &manifest.Ref{Tape: c.Tape.ID, Path: c.Entry.Path},
			}, nil
		}
	}
	return nil, nil
}

// openSource opens the source file and makes sure it is still the regular
// file found while planning, not something swapped in since (for example a
// symlink to a file the user should not be able to archive).
func openSource(j job) (*os.File, error) {
	f, err := os.Open(j.src)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() || !os.SameFile(st, j.info) {
		f.Close()
		return nil, errors.New("source file was replaced after it was scanned")
	}
	return f, nil
}

// plan walks src and returns the regular files to archive in lexical order.
func plan(src, prefix string, log io.Writer) ([]job, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return nil, err
	}
	if info.Mode().IsRegular() {
		return []job{{src: src, rel: prefix, info: info}}, nil
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a regular file or directory", src)
	}

	var jobs []job
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			fmt.Fprintf(log, "WARNING:   skipping non-regular file %s\n", p)
			return nil
		}
		if strings.ContainsAny(p, "\n\r") {
			fmt.Fprintf(log, "WARNING:   skipping file with newline in name %q\n", p)
			return nil
		}
		if strings.HasSuffix(p, PartialSuffix) {
			return fmt.Errorf("%s: source file name uses reserved suffix %s", p, PartialSuffix)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		jobs = append(jobs, job{src: p, rel: path.Join(prefix, filepath.ToSlash(rel)), info: info})
		return nil
	})
	return jobs, err
}

// fileState bundles what archiveFile needs for one file.
type fileState struct {
	opts  PutOptions
	root  *os.Root
	j     job
	jpath string // resume journal
	ppath string // staged parity
	m     int    // parity shards per stripe, 0 for none
	// check is called once it is known how much of the file is already on
	// tape and will be reused, to make sure the rest fits.
	check func(reused int64) error
}

// archived is the result of archiveFile.
type archived struct {
	entry   manifest.Entry
	chunks  *manifest.Chunks
	parity  *manifest.Parity
	resumed int64
	note    string // something the user should know, such as lost parity
}

// archiveFile streams one file to the tape.
//
// Data goes to a partial file first and is renamed only once complete, so a
// crash never leaves a truncated file under its final name. Progress is
// checkpointed to the journal. Parity is computed while streaming and
// staged locally; it reaches the tape with the next manifest segment.
func archiveFile(fs fileState) (*archived, error) {
	opts, root, j := fs.opts, fs.root, fs.j
	dst := filepath.FromSlash(j.rel)
	partial := dst + PartialSuffix
	layout := parity.ForFile(j.info.Size(), opts.ChunkSize, fs.m)
	hdr := journalHeader{Source: j.src, Path: j.rel, Size: j.info.Size(), MTime: j.info.ModTime(), ChunkSize: opts.ChunkSize, ParityM: fs.m}

	oldHdr, points, err := loadJournal(fs.jpath)
	if err != nil {
		return nil, err
	}
	journalValid := oldHdr != nil && sameHeader(*oldHdr, hdr)

	if _, err := root.Lstat(dst); err == nil {
		// A previous run may have renamed the file and stopped before
		// recording it. Only the source has to match: the chunk size and
		// parity of the finished file are taken from its journal, whatever
		// the settings of this run.
		if oldHdr != nil && sameSource(*oldHdr, hdr) && len(points) > 0 {
			p := points[len(points)-1]
			cs := oldHdr.ChunkSize
			if p.sha256 != "" && p.offset == j.info.Size() && cs >= manifest.MinChunkSize && cs <= manifest.MaxChunkSize &&
				int64(len(p.chunks)) == (p.offset+cs-1)/cs {
				if st, err := root.Lstat(dst); err == nil && st.Mode().IsRegular() && st.Size() == p.offset {
					res := &archived{entry: entryFor(j, p.sha256), chunks: chunksFor(j, cs, p.chunks), resumed: p.offset}
					if p.parity != nil && stagedParityOK(fs.ppath, p.parity) {
						res.parity = p.parity
					} else if oldHdr.ParityM > 0 {
						res.note = "staged parity was lost; this file has no parity"
					}
					return res, nil
				}
			}
		}
		return nil, fmt.Errorf("%s exists on tape but is not in the manifest", j.rel)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// Small files are buffered for their parity and are cheap to redo, so
	// they never resume.
	var start *resumePoint
	if journalValid && (layout == nil || layout.Scheme == parity.SchemeWindow) {
		var accept func(*resumePoint) bool
		if layout != nil && len(points) > 0 {
			// Hash lists are cumulative, so the staged file is checked once
			// against the newest list; older points only compare lengths.
			valid := stagedValidPrefix(fs.ppath, layout.ShardSize, points[len(points)-1].parityHashes)
			accept = func(p *resumePoint) bool { return parityResumable(layout, p, valid) }
		}
		if start, err = pickResumePoint(root, partial, points, j.info.Size(), opts.ChunkSize, accept); err != nil {
			return nil, err
		}
	}

	if fs.check != nil {
		var reused int64
		if start != nil {
			reused = start.offset
		}
		if err := fs.check(reused); err != nil {
			return nil, err
		}
	}

	in, err := openSource(j)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	t := &transfer{opts: opts, in: in, h: sha256.New()}
	if start != nil {
		if err := t.h.(encoding.BinaryUnmarshaler).UnmarshalBinary(start.state); err != nil {
			return nil, fmt.Errorf("restoring checkpoint: %w", err)
		}
		if t.out, err = root.OpenFile(partial, os.O_WRONLY, 0); err != nil {
			return nil, err
		}
		t.offset, t.chunks = start.offset, start.chunks
		if err := t.out.Truncate(t.offset); err == nil {
			_, err = t.out.Seek(t.offset, io.SeekStart)
		}
		if err == nil {
			_, err = in.Seek(t.offset, io.SeekStart)
		}
		if err != nil {
			t.out.Close()
			return nil, err
		}
	} else {
		if err := root.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := root.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if t.out, err = root.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err != nil {
			return nil, err
		}
	}
	defer func() {
		if t.out != nil {
			t.out.Close()
		}
	}()

	if layout != nil {
		if err := t.startParity(fs.ppath, layout, start); err != nil {
			return nil, err
		}
		defer t.staging.Close()
	}
	resumed := t.offset

	if t.jr, err = writeJournal(fs.jpath, hdr, start); err != nil {
		return nil, err
	}
	defer t.jr.close()

	t.prog = startProgress(opts.Progress, j.info.Size())
	t.prog.done.Store(t.offset)
	err = t.copy()
	t.prog.finish()
	if err != nil {
		return nil, err
	}

	after, err := in.Stat()
	if err != nil {
		return nil, err
	}
	if t.offset != j.info.Size() || after.Size() != j.info.Size() || !after.ModTime().Equal(j.info.ModTime()) {
		return nil, errors.New("source changed while it was being archived")
	}

	if err := t.out.Sync(); err != nil {
		return nil, err
	}
	err = t.out.Close()
	t.out = nil
	if err != nil {
		return nil, err
	}

	var par *manifest.Parity
	if layout != nil {
		if par, err = t.finishParity(j.rel); err != nil {
			return nil, fmt.Errorf("parity: %w", err)
		}
	}

	state, err := t.h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(t.h.Sum(nil))
	final := journalRecord{Offset: t.offset, State: state, Chunks: pendingChunks(t.chunks, t.jr), SHA256: sum, Parity: par}
	if err := t.jr.append(final); err != nil {
		return nil, err
	}
	if err := root.Chtimes(partial, j.info.ModTime(), j.info.ModTime()); err != nil {
		return nil, err
	}
	if err := root.Rename(partial, dst); err != nil {
		return nil, err
	}
	if err := syncTapeDir(root, filepath.Dir(dst)); err != nil {
		return nil, err
	}
	return &archived{entry: entryFor(j, sum), chunks: chunksFor(j, opts.ChunkSize, t.chunks), parity: par, resumed: resumed}, nil
}

// transfer is one file copy in progress.
type transfer struct {
	opts   PutOptions
	in     io.Reader
	out    *os.File
	h      hash.Hash
	prog   *progress
	jr     *journal
	offset int64
	chunks []string

	layout  *parity.Layout
	staging *os.File
	enc     *parity.WindowEncoder // large files
	small   *bytes.Buffer         // small files, buffered whole
}

func (t *transfer) startParity(ppath string, l *parity.Layout, start *resumePoint) error {
	t.layout = l
	if err := os.MkdirAll(filepath.Dir(ppath), 0o755); err != nil {
		return err
	}
	var err error
	if start != nil {
		if t.staging, err = os.OpenFile(ppath, os.O_WRONLY, 0); err != nil {
			return err
		}
		end := int64(len(start.parityHashes)) * l.ShardSize
		if err := t.staging.Truncate(end); err != nil {
			return err
		}
		if _, err := t.staging.Seek(end, io.SeekStart); err != nil {
			return err
		}
	} else {
		if t.staging, err = os.OpenFile(ppath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644); err != nil {
			return err
		}
		if err := manifest.SyncDir(filepath.Dir(ppath)); err != nil {
			return err
		}
	}
	if l.Scheme == parity.SchemeSmall {
		t.small = bytes.NewBuffer(make([]byte, 0, l.Size))
		return nil
	}
	if t.enc, err = parity.NewWindowEncoder(l, t.offset/l.ShardSize, t.staging); err != nil {
		return err
	}
	if start != nil {
		t.enc.SetHashes(start.parityHashes)
	}
	return nil
}

func (t *transfer) finishParity(rel string) (*manifest.Parity, error) {
	par := &manifest.Parity{Path: rel, Layout: *t.layout}
	if t.small != nil {
		data, dataHashes, parityHashes, err := parity.EncodeSmall(t.layout, t.small.Bytes())
		if err != nil {
			return nil, err
		}
		if _, err := t.staging.Write(data); err != nil {
			return nil, err
		}
		par.Hashes, par.DataHashes = parityHashes, dataHashes
	} else {
		par.Hashes = t.enc.Hashes()
	}
	if int64(len(par.Hashes)) != t.layout.ParityShards() {
		return nil, errors.New("parity incomplete")
	}
	if err := t.staging.Sync(); err != nil {
		return nil, err
	}
	return par, par.Validate()
}

// readAhead is how many chunks are read ahead of the writer, so a slow
// source read never leaves the tape drive waiting.
const readAhead = 4

type readChunk struct {
	buf []byte
	n   int
	err error
}

// readChunks reads r in chunkSize pieces in its own goroutine. Buffers are
// handed back through free once used. Closing stop ends the reader early.
func readChunks(r io.Reader, chunkSize int64, stop <-chan struct{}) (<-chan readChunk, chan<- []byte) {
	out := make(chan readChunk, readAhead)
	free := make(chan []byte, readAhead+2)
	for range readAhead + 2 {
		free <- make([]byte, chunkSize)
	}
	go func() {
		defer close(out)
		for {
			var buf []byte
			select {
			case buf = <-free:
			case <-stop:
				return
			}
			n, err := io.ReadFull(r, buf)
			select {
			case out <- readChunk{buf: buf, n: n, err: err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return out, free
}

// copy copies the source to the tape one chunk at a time. For each chunk
// the tape write, the file hash, the chunk hash and the parity update run in
// parallel, while the next chunks are already being read. SHA-256 is the
// slowest step on CPUs without SHA instructions, so running the two hashes
// side by side matters for keeping the drive streaming.
//
// A journal checkpoint is written every opts.CheckpointEvery bytes once the
// data up to that point is synced. With parity, checkpoints wait for a
// window boundary so no parity state has to be saved.
func (t *transfer) copy() error {
	stop := make(chan struct{})
	defer close(stop)
	chunks, free := readChunks(t.in, t.opts.ChunkSize, stop)

	lastCheckpoint := t.offset
	for rc := range chunks {
		if rc.n > 0 {
			if err := t.process(rc.buf[:rc.n]); err != nil {
				return err
			}
		}
		free <- rc.buf
		if rc.err == io.EOF || rc.err == io.ErrUnexpectedEOF {
			return nil
		}
		if rc.err != nil {
			return rc.err
		}
		if t.small == nil && t.offset-lastCheckpoint >= t.opts.CheckpointEvery && (t.enc == nil || t.enc.Next()%int64(t.layout.K*t.layout.D) == 0) {
			if err := t.checkpoint(); err != nil {
				return err
			}
			lastCheckpoint = t.offset
			if err := hook("checkpoint", t.offset); err != nil {
				return err
			}
		}
	}
	return errors.New("source reader stopped unexpectedly")
}

// process handles one chunk and returns once every step is done.
func (t *transfer) process(b []byte) error {
	var wg sync.WaitGroup
	var writeErr, parityErr error
	var chunkSum [sha256.Size]byte
	wg.Go(func() { _, writeErr = t.out.Write(b) })
	wg.Go(func() { t.h.Write(b) })
	wg.Go(func() { chunkSum = sha256.Sum256(b) })
	if t.enc != nil {
		wg.Go(func() { parityErr = t.enc.Add(b) })
	}
	if t.small != nil {
		t.small.Write(b)
	}
	t.prog.Write(b)
	wg.Wait()
	if err := errors.Join(writeErr, parityErr); err != nil {
		return err
	}
	t.chunks = append(t.chunks, hex.EncodeToString(chunkSum[:]))
	t.offset += int64(len(b))
	return nil
}

func (t *transfer) checkpoint() error {
	if err := t.out.Sync(); err != nil {
		return err
	}
	rec := journalRecord{Offset: t.offset, Chunks: pendingChunks(t.chunks, t.jr)}
	if t.enc != nil {
		if err := t.staging.Sync(); err != nil {
			return err
		}
		all := t.enc.Hashes()
		rec.ParityHashes = all[t.jr.parityRecorded:]
		t.jr.parityRecorded = len(all)
	}
	state, err := t.h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return err
	}
	rec.State = state
	return t.jr.append(rec)
}

// parityResumable reports whether a resume point lies on a window boundary
// and the staged parity up to it is intact.
// valid is the number of leading staged parity shards that match their
// recorded hashes.
func parityResumable(l *parity.Layout, p *resumePoint, valid int) bool {
	chunks := p.offset / l.ShardSize
	if p.offset%l.ShardSize != 0 || chunks%int64(l.K*l.D) != 0 {
		return false
	}
	n := len(p.parityHashes)
	return int64(n) == chunks/int64(l.K)*int64(l.M) && n <= valid
}

// stagedValidPrefix returns how many leading shards of the staged parity
// file match hashes.
func stagedValidPrefix(ppath string, shardSize int64, hashes []string) int {
	f, err := os.Open(ppath)
	if err != nil {
		return 0
	}
	defer f.Close()
	buf := make([]byte, shardSize)
	for i, want := range hashes {
		if _, err := io.ReadFull(f, buf); err != nil {
			return i
		}
		got := sha256.Sum256(buf)
		if hex.EncodeToString(got[:]) != want {
			return i
		}
	}
	return len(hashes)
}

// stagedParityOK reports whether the staged parity file holds exactly the
// parity described by p.
func stagedParityOK(ppath string, p *manifest.Parity) bool {
	st, err := os.Stat(ppath)
	if err != nil || st.Size() != p.Layout.ParitySize() {
		return false
	}
	return stagedShardsOK(ppath, p.Layout.ShardSize, p.Hashes)
}

func stagedShardsOK(ppath string, shardSize int64, hashes []string) bool {
	f, err := os.Open(ppath)
	if err != nil {
		return len(hashes) == 0
	}
	defer f.Close()
	buf := make([]byte, shardSize)
	for _, want := range hashes {
		if _, err := io.ReadFull(f, buf); err != nil {
			return false
		}
		got := sha256.Sum256(buf)
		if hex.EncodeToString(got[:]) != want {
			return false
		}
	}
	return true
}

// pendingChunks returns the chunk hashes not yet recorded in the journal.
func pendingChunks(chunks []string, jr *journal) []string {
	p := chunks[jr.recorded:]
	jr.recorded = len(chunks)
	return p
}

// pickResumePoint returns the latest checkpoint whose data is fully present
// in the partial file, after checking that its last chunk reads back
// correctly and that accept (if set) agrees, for example that the staged
// parity up to it is intact. Older checkpoints are tried in turn. LTFS may lose data written after its last index update, so the
// partial file can be shorter than the newest checkpoint.
func pickResumePoint(root *os.Root, partial string, points []resumePoint, size, chunkSize int64, accept func(*resumePoint) bool) (*resumePoint, error) {
	st, err := root.Lstat(partial)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil
	}
	f, err := root.Open(partial)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	for i := len(points) - 1; i >= 0; i-- {
		p := points[i]
		// The journal is a local file but may be damaged; never trust its
		// offsets for allocations or seeks without checking them.
		if p.offset <= 0 || p.offset > st.Size() || p.offset > size ||
			int64(len(p.chunks)) != (p.offset+chunkSize-1)/chunkSize {
			continue
		}
		last := int64(len(p.chunks)-1) * chunkSize
		buf := make([]byte, p.offset-last)
		if _, err := f.ReadAt(buf, last); err != nil {
			continue
		}
		c := sha256.Sum256(buf)
		if hex.EncodeToString(c[:]) == p.chunks[len(p.chunks)-1] && (accept == nil || accept(&p)) {
			return &p, nil
		}
	}
	return nil, nil
}

func sameSource(a, b journalHeader) bool {
	return a.Source == b.Source && a.Path == b.Path && a.Size == b.Size && a.MTime.Equal(b.MTime)
}

func sameHeader(a, b journalHeader) bool {
	return a.Source == b.Source && a.Path == b.Path && a.Size == b.Size && a.MTime.Equal(b.MTime) &&
		a.ChunkSize == b.ChunkSize && a.ParityM == b.ParityM
}

func entryFor(j job, sum string) manifest.Entry {
	return manifest.Entry{
		Path:       j.rel,
		Size:       j.info.Size(),
		SHA256:     sum,
		MTime:      j.info.ModTime(),
		Source:     j.src,
		ArchivedAt: time.Now().UTC(),
	}
}

func chunksFor(j job, size int64, chunks []string) *manifest.Chunks {
	if chunks == nil {
		chunks = []string{}
	}
	return &manifest.Chunks{Path: j.rel, ChunkSize: size, SHA256: chunks}
}
