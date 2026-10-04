// Package archive implements the archive workflow: streaming files onto a
// mounted tape while hashing them, and verifying them by reading them back.
package archive

import (
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
	"time"

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/ltfs"
	"github.com/Knight1/tapemanager/internal/manifest"
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
	// DefaultDedupMinSize is the smallest file considered for deduplication.
	DefaultDedupMinSize = 1 << 20
	// DefaultFlushEvery is how much archived data accumulates before its
	// manifest records are written to the tape as one segment. Larger
	// values mean fewer, bigger metadata files on tape.
	DefaultFlushEvery = 100 << 30
)

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
	Log      io.Writer        // per-file output
	Progress io.Writer        // progress bar output, nil to disable

	ChunkSize       int64 // default DefaultChunkSize
	CheckpointEvery int64 // default DefaultCheckpointEvery
	DedupMinSize    int64 // default DefaultDedupMinSize
	FlushEvery      int64 // default DefaultFlushEvery
}

// PutSummary reports what Put did.
type PutSummary struct {
	Tape     *manifest.Volume
	Files    int
	Skipped  int
	Deduped  int
	Bytes    int64
	Resumed  int64 // bytes not rewritten thanks to a resume checkpoint
	Duration time.Duration
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
		onTape := make(map[string]string, len(existing))
		for _, e := range existing {
			onTape[e.Path] = e.SHA256
		}
		var entries, written []manifest.Entry
		var chunks []manifest.Chunks
		for _, r := range pend.records {
			written = append(written, r.Entry)
			// A crash after the segment was written but before the
			// pending log was cleared leaves records already on tape.
			if sha, ok := onTape[r.Entry.Path]; ok {
				if sha != r.Entry.SHA256 {
					return fmt.Errorf("pending record for %s conflicts with the tape manifest", r.Entry.Path)
				}
				continue
			}
			entries = append(entries, r.Entry)
			if r.Chunks != nil {
				chunks = append(chunks, *r.Chunks)
			}
		}
		all := append(existing[:len(existing):len(existing)], entries...)
		if err := tape.WriteSegment(entries, chunks, all); err != nil {
			return fmt.Errorf("writing manifest segment: %w", err)
		}
		existing = all
		// Recorded after the tape write and before clearing, so a crash in
		// between repeats it rather than losing it. Duplicates are harmless.
		if err := opts.Catalog.RecordWritten(vol.ID, written); err != nil {
			return fmt.Errorf("recording written files: %w", err)
		}
		return pend.clear()
	}

	if n := len(pend.records); n > 0 {
		fmt.Fprintf(opts.Log, "RECOVERING: writing %d records from an interrupted run to the tape manifest\n", n)
		if err := flush(); err != nil {
			return sum, err
		}
	}

	archived := make(map[string]manifest.Entry, len(existing))
	for _, e := range existing {
		archived[e.Path] = e
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
		archived[r.Entry.Path] = r.Entry
		if pend.bytes >= opts.FlushEvery {
			return flush()
		}
		return nil
	}

	journalDir := opts.Catalog.JournalDir(vol.ID)
	for _, j := range jobs {
		if e, ok := archived[j.rel]; ok {
			if e.Size == j.info.Size() && e.MTime.Equal(j.info.ModTime()) {
				fmt.Fprintf(opts.Log, "SKIPPING:  %s (already archived)\n", j.src)
				sum.Skipped++
				continue
			}
			return sum, fmt.Errorf("%s: a different version is already archived as %s", j.src, j.rel)
		}

		if opts.Dedup && j.info.Size() >= opts.DedupMinSize && len(dedup[j.info.Size()]) > 0 {
			e, err := findDuplicate(j, dedup[j.info.Size()])
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

		fmt.Fprintf(opts.Log, "ARCHIVING: %s\n       %s\n", j.src, FormatBytes(j.info.Size()))
		jpath := journalPath(journalDir, j.rel)
		e, chunks, resumed, err := archiveFile(opts, tape.Root(), j, jpath)
		if err != nil {
			return sum, fmt.Errorf("%s: %w", j.src, err)
		}
		if resumed > 0 {
			fmt.Fprintf(opts.Log, "       resumed at %s\n", FormatBytes(resumed))
		}
		if err := hook("manifest", e.Size); err != nil {
			return sum, err
		}
		if err := record(pendingRecord{Entry: e, Chunks: chunks}); err != nil {
			return sum, err
		}
		if err := os.Remove(jpath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return sum, err
		}
		fmt.Fprintf(opts.Log, "       SHA256: %s\n\n", e.SHA256)
		if opts.Dedup {
			dedup[e.Size] = append(dedup[e.Size], catalog.Hit{Tape: catalog.Tape{Volume: *vol}, Entry: e})
		}
		sum.Files++
		sum.Bytes += e.Size
		sum.Resumed += resumed
	}
	return sum, nil
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

// archiveFile streams one file to the tape and returns its manifest entry,
// chunk hashes, and how many bytes were skipped by resuming.
//
// Data goes to a partial file first and is renamed only once complete, so a
// crash never leaves a truncated file under its final name. Progress is
// checkpointed to the journal at jpath.
func archiveFile(opts PutOptions, root *os.Root, j job, jpath string) (manifest.Entry, *manifest.Chunks, int64, error) {
	var none manifest.Entry
	dst := filepath.FromSlash(j.rel)
	partial := dst + PartialSuffix
	hdr := journalHeader{Source: j.src, Path: j.rel, Size: j.info.Size(), MTime: j.info.ModTime(), ChunkSize: opts.ChunkSize}

	oldHdr, points, err := loadJournal(jpath)
	if err != nil {
		return none, nil, 0, err
	}
	journalValid := oldHdr != nil && sameHeader(*oldHdr, hdr)

	if _, err := root.Lstat(dst); err == nil {
		// A previous run may have renamed the file and stopped before
		// writing the manifest entry.
		if journalValid && len(points) > 0 {
			if p := points[len(points)-1]; p.sha256 != "" && p.offset == j.info.Size() {
				if st, err := root.Lstat(dst); err == nil && st.Mode().IsRegular() && st.Size() == p.offset {
					return entryFor(j, p.sha256), chunksFor(j, opts.ChunkSize, p.chunks), p.offset, nil
				}
			}
		}
		return none, nil, 0, fmt.Errorf("%s exists on tape but is not in the manifest", j.rel)
	} else if !errors.Is(err, os.ErrNotExist) {
		return none, nil, 0, err
	}

	var start *resumePoint
	if journalValid {
		if start, err = pickResumePoint(root, partial, points, j.info.Size(), opts.ChunkSize); err != nil {
			return none, nil, 0, err
		}
	}

	in, err := openSource(j)
	if err != nil {
		return none, nil, 0, err
	}
	defer in.Close()

	h := sha256.New()
	var out *os.File
	var chunks []string
	var offset int64
	if start != nil {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(start.state); err != nil {
			return none, nil, 0, fmt.Errorf("restoring checkpoint: %w", err)
		}
		if out, err = root.OpenFile(partial, os.O_WRONLY, 0); err != nil {
			return none, nil, 0, err
		}
		offset, chunks = start.offset, start.chunks
		if err := out.Truncate(offset); err == nil {
			_, err = out.Seek(offset, io.SeekStart)
		}
		if err == nil {
			_, err = in.Seek(offset, io.SeekStart)
		}
		if err != nil {
			out.Close()
			return none, nil, 0, err
		}
	} else {
		if err := root.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
			return none, nil, 0, err
		}
		if err := root.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return none, nil, 0, err
		}
		if out, err = root.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err != nil {
			return none, nil, 0, err
		}
	}
	resumed := offset

	ok := false
	defer func() {
		if !ok {
			out.Close()
		}
	}()

	jr, err := writeJournal(jpath, hdr, start)
	if err != nil {
		return none, nil, 0, err
	}
	defer jr.close()

	prog := startProgress(opts.Progress, j.info.Size())
	prog.done.Store(offset)
	err = copyChunks(in, out, h, prog, &offset, &chunks, opts, jr)
	prog.finish()
	if err != nil {
		return none, nil, 0, err
	}

	after, err := in.Stat()
	if err != nil {
		return none, nil, 0, err
	}
	if offset != j.info.Size() || after.Size() != j.info.Size() || !after.ModTime().Equal(j.info.ModTime()) {
		return none, nil, 0, errors.New("source changed while it was being archived")
	}

	if err := out.Sync(); err != nil {
		return none, nil, 0, err
	}
	ok = true
	if err := out.Close(); err != nil {
		return none, nil, 0, err
	}
	state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return none, nil, 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := jr.append(journalRecord{Offset: offset, State: state, Chunks: pendingChunks(chunks, jr), SHA256: sum}); err != nil {
		return none, nil, 0, err
	}
	if err := root.Chtimes(partial, j.info.ModTime(), j.info.ModTime()); err != nil {
		return none, nil, 0, err
	}
	if err := root.Rename(partial, dst); err != nil {
		return none, nil, 0, err
	}
	return entryFor(j, sum), chunksFor(j, opts.ChunkSize, chunks), resumed, nil
}

// copyChunks copies in to out one chunk at a time, hashing each chunk and the
// whole stream, and writes a journal checkpoint every opts.CheckpointEvery
// bytes once the data up to that point is synced.
func copyChunks(in io.Reader, out *os.File, h hash.Hash, prog io.Writer, offset *int64, chunks *[]string, opts PutOptions, jr *journal) error {
	buf := make([]byte, opts.ChunkSize)
	lastCheckpoint := *offset
	for {
		n, rerr := io.ReadFull(in, buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				return err
			}
			h.Write(buf[:n])
			prog.Write(buf[:n])
			c := sha256.Sum256(buf[:n])
			*chunks = append(*chunks, hex.EncodeToString(c[:]))
			*offset += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		if *offset-lastCheckpoint >= opts.CheckpointEvery {
			if err := out.Sync(); err != nil {
				return err
			}
			state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				return err
			}
			if err := jr.append(journalRecord{Offset: *offset, State: state, Chunks: pendingChunks(*chunks, jr)}); err != nil {
				return err
			}
			lastCheckpoint = *offset
			if err := hook("checkpoint", *offset); err != nil {
				return err
			}
		}
	}
}

// pendingChunks returns the chunk hashes not yet recorded in the journal.
func pendingChunks(chunks []string, jr *journal) []string {
	p := chunks[jr.recorded:]
	jr.recorded = len(chunks)
	return p
}

// pickResumePoint returns the latest checkpoint whose data is fully present
// in the partial file, after checking that its last chunk reads back
// correctly. LTFS may lose data written after its last index update, so the
// partial file can be shorter than the newest checkpoint.
func pickResumePoint(root *os.Root, partial string, points []resumePoint, size, chunkSize int64) (*resumePoint, error) {
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
		if hex.EncodeToString(c[:]) == p.chunks[len(p.chunks)-1] {
			return &p, nil
		}
	}
	return nil, nil
}

func sameHeader(a, b journalHeader) bool {
	return a.Source == b.Source && a.Path == b.Path && a.Size == b.Size && a.MTime.Equal(b.MTime) && a.ChunkSize == b.ChunkSize
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
