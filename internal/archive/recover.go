package archive

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/ltfs"
	"github.com/Knight1/tapemanager/internal/manifest"
)

// RecoverOptions configures Recover.
type RecoverOptions struct {
	TapeRoot string
	Label    string // tape label, used only if the tape has no tapemgr volume yet
	Catalog  *catalog.Catalog
	Log      io.Writer
	Progress io.Writer

	ChunkSize  int64 // default DefaultChunkSize
	FlushEvery int64 // default DefaultFlushEvery
}

// RecoverResult reports what Recover did.
type RecoverResult struct {
	Tape     *manifest.Volume
	Files    int
	Bytes    int64
	Duration time.Duration
}

// Recover records files that are on the tape but not in its manifest: files
// whose manifest records were lost, or files copied to the tape by other
// tools. Each is read back from the tape and hashed. The resulting entries
// are marked as recovered because the hash proves only what the tape holds,
// not that it matches any source.
//
// Files are read in tape order when LTFS reports start blocks, to avoid
// seeking back and forth.
func Recover(opts RecoverOptions) (res RecoverResult, err error) {
	start := time.Now()
	if opts.Catalog == nil {
		return res, errors.New("catalog is required")
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = DefaultChunkSize
	}
	if opts.FlushEvery <= 0 {
		opts.FlushEvery = DefaultFlushEvery
	}

	tape, err := manifest.Open(opts.TapeRoot)
	if err != nil {
		return res, err
	}
	defer tape.Close()
	vol, err := tape.InitVolume(opts.Label, ltfs.VolumeUUID(opts.TapeRoot))
	if err != nil {
		return res, err
	}
	res.Tape = vol

	// Files with pending records are complete and known; recovering them
	// would record them twice.
	if n, err := pendingCount(opts.Catalog.PendingPath(vol.ID)); err != nil {
		return res, err
	} else if n > 0 {
		return res, fmt.Errorf("%d records from an interrupted run are pending; run 'archive put' first to write them", n)
	}

	existing, err := tape.Entries()
	if err != nil {
		return res, err
	}
	known := make(map[string]bool, len(existing))
	for _, e := range existing {
		known[e.Path] = true
	}

	type orphan struct {
		rel   string
		block int64
		known bool // block is known
	}
	var orphans []orphan
	err = filepath.WalkDir(opts.TapeRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(opts.TapeRoot, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == manifest.Dir {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == manifest.SumsName || known[rel] {
			return nil
		}
		if strings.HasSuffix(rel, PartialSuffix) {
			fmt.Fprintf(opts.Log, "IGNORED:   %s (incomplete transfer, finish it with 'archive put')\n", rel)
			return nil
		}
		if !d.Type().IsRegular() {
			fmt.Fprintf(opts.Log, "IGNORED:   %s (not a regular file)\n", rel)
			return nil
		}
		if !manifest.ValidPath(rel) {
			fmt.Fprintf(opts.Log, "IGNORED:   %q (unsupported file name)\n", rel)
			return nil
		}
		block, ok := ltfs.StartBlock(p)
		orphans = append(orphans, orphan{rel: rel, block: block, known: ok})
		return nil
	})
	if err != nil {
		return res, err
	}
	sort.SliceStable(orphans, func(i, j int) bool {
		a, b := orphans[i], orphans[j]
		if a.known != b.known {
			return a.known
		}
		return a.known && a.block < b.block
	})

	var batch []manifest.Entry
	var chunks []manifest.Chunks
	var batchBytes int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		all := append(existing[:len(existing):len(existing)], batch...)
		if err := tape.WriteSegment(manifest.Segment{Entries: batch, Chunks: chunks}, all); err != nil {
			return fmt.Errorf("writing manifest segment: %w", err)
		}
		existing = all
		batch, chunks, batchBytes = nil, nil, 0
		return nil
	}
	defer func() {
		if ferr := flush(); ferr != nil {
			err = errors.Join(err, ferr)
		}
		if _, cerr := opts.Catalog.Import(opts.TapeRoot); cerr != nil {
			err = errors.Join(err, fmt.Errorf("updating catalog: %w", cerr))
		}
		res.Duration = time.Since(start)
	}()

	for _, o := range orphans {
		fmt.Fprintf(opts.Log, "RECOVERING: %s\n", o.rel)
		e, c, err := hashTapeFile(tape.Root(), o.rel, opts.ChunkSize, opts.Progress)
		if err != nil {
			return res, fmt.Errorf("%s: %w", o.rel, err)
		}
		fmt.Fprintf(opts.Log, "       SHA256: %s\n\n", e.SHA256)
		batch = append(batch, e)
		chunks = append(chunks, c)
		batchBytes += e.Size
		res.Files++
		res.Bytes += e.Size
		if batchBytes >= opts.FlushEvery {
			if err := flush(); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

func hashTapeFile(root *os.Root, rel string, chunkSize int64, progressOut io.Writer) (manifest.Entry, manifest.Chunks, error) {
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return manifest.Entry{}, manifest.Chunks{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return manifest.Entry{}, manifest.Chunks{}, err
	}
	if !st.Mode().IsRegular() {
		return manifest.Entry{}, manifest.Chunks{}, errors.New("not a regular file")
	}
	prog := startProgress(progressOut, st.Size())
	sum, list, n, err := hashStream(f, chunkSize, prog)
	prog.finish()
	if err != nil {
		return manifest.Entry{}, manifest.Chunks{}, err
	}
	if n != st.Size() {
		return manifest.Entry{}, manifest.Chunks{}, fmt.Errorf("read %d bytes, file size is %d", n, st.Size())
	}
	if list == nil {
		list = []string{}
	}
	e := manifest.Entry{
		Path:       rel,
		Size:       n,
		SHA256:     sum,
		MTime:      st.ModTime(),
		ArchivedAt: time.Now().UTC(),
		Recovered:  true,
	}
	return e, manifest.Chunks{Path: rel, ChunkSize: chunkSize, SHA256: list}, nil
}
