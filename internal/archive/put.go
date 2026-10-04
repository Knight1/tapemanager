// Package archive implements the archive workflow: streaming files onto a
// mounted tape while hashing them, and verifying them by reading them back.
package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Knight1/tapemanager/internal/manifest"
)

// PartialSuffix marks a file on tape whose transfer has not completed.
const PartialSuffix = ".tapemgr-partial"

// copyBufferSize is the read/write chunk size. Large sequential writes keep
// the drive streaming instead of shoe-shining.
const copyBufferSize = 4 << 20

// PutOptions configures Put.
type PutOptions struct {
	TapeRoot string    // LTFS mount point
	Source   string    // file or directory to archive
	Prefix   string    // destination directory on tape; defaults to the source base name
	Log      io.Writer // per-file output
	Progress io.Writer // progress bar output, nil to disable
}

// PutSummary reports what Put did.
type PutSummary struct {
	Files    int
	Skipped  int
	Bytes    int64
	Duration time.Duration
}

type job struct {
	src  string
	rel  string // slash separated, relative to the tape root
	info fs.FileInfo
}

// Put archives opts.Source onto the tape. Files are written strictly one
// after another in lexical order. Each file's SHA-256 is computed from the
// same bytes that are written, and a manifest entry is appended only after
// the file has been synced and renamed into place.
//
// Files already present in the manifest with the same size and mtime are
// skipped, so rerunning an interrupted Put continues where it stopped.
func Put(opts PutOptions) (PutSummary, error) {
	start := time.Now()
	var sum PutSummary

	src, err := filepath.Abs(opts.Source)
	if err != nil {
		return sum, err
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = filepath.Base(src)
	}
	prefix = path.Clean(filepath.ToSlash(prefix))
	if prefix == "." || prefix == ".." || strings.HasPrefix(prefix, "../") || path.IsAbs(prefix) {
		return sum, fmt.Errorf("invalid destination prefix %q", prefix)
	}

	jobs, err := plan(src, prefix, opts.Log)
	if err != nil {
		return sum, err
	}

	existing, err := manifest.Load(opts.TapeRoot)
	if err != nil {
		return sum, err
	}
	archived := make(map[string]manifest.Entry, len(existing))
	for _, e := range existing {
		archived[e.Path] = e
	}

	w, err := manifest.OpenWriter(opts.TapeRoot)
	if err != nil {
		return sum, err
	}
	defer w.Close()

	for _, j := range jobs {
		if e, ok := archived[j.rel]; ok {
			if e.Size == j.info.Size() && e.MTime.Equal(j.info.ModTime()) {
				fmt.Fprintf(opts.Log, "SKIPPING:  %s (already archived)\n", j.src)
				sum.Skipped++
				continue
			}
			return sum, fmt.Errorf("%s: a different version is already archived as %s", j.src, j.rel)
		}

		fmt.Fprintf(opts.Log, "ARCHIVING: %s\n       %s\n", j.src, FormatBytes(j.info.Size()))
		e, err := archiveFile(opts.TapeRoot, j, opts.Progress)
		if err != nil {
			return sum, fmt.Errorf("%s: %w", j.src, err)
		}
		if err := w.Append(e); err != nil {
			return sum, fmt.Errorf("writing manifest: %w", err)
		}
		fmt.Fprintf(opts.Log, "       SHA256: %s\n\n", e.SHA256)
		sum.Files++
		sum.Bytes += e.Size
	}

	sum.Duration = time.Since(start)
	return sum, w.Close()
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

// archiveFile streams one file to the tape and returns its manifest entry.
// Data goes to a partial file first and is renamed only once complete, so a
// crash never leaves a truncated file under its final name.
func archiveFile(tapeRoot string, j job, progressOut io.Writer) (manifest.Entry, error) {
	dst := filepath.Join(tapeRoot, filepath.FromSlash(j.rel))
	if _, err := os.Lstat(dst); err == nil {
		return manifest.Entry{}, fmt.Errorf("%s exists on tape but is not in the manifest", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return manifest.Entry{}, err
	}

	partial := dst + PartialSuffix
	if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return manifest.Entry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return manifest.Entry{}, err
	}

	in, err := os.Open(j.src)
	if err != nil {
		return manifest.Entry{}, err
	}
	defer in.Close()

	out, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return manifest.Entry{}, err
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(partial)
		}
	}()

	h := sha256.New()
	prog := startProgress(progressOut, j.info.Size())
	// Wrap the reader so io.CopyBuffer uses our buffer instead of a
	// WriterTo fast path with small chunks.
	n, err := io.CopyBuffer(io.MultiWriter(out, h, prog), struct{ io.Reader }{in}, make([]byte, copyBufferSize))
	prog.finish()
	if err != nil {
		return manifest.Entry{}, err
	}

	after, err := in.Stat()
	if err != nil {
		return manifest.Entry{}, err
	}
	if n != j.info.Size() || after.Size() != j.info.Size() || !after.ModTime().Equal(j.info.ModTime()) {
		return manifest.Entry{}, errors.New("source changed while it was being archived")
	}

	if err := out.Sync(); err != nil {
		return manifest.Entry{}, err
	}
	if err := out.Close(); err != nil {
		return manifest.Entry{}, err
	}
	if err := os.Chtimes(partial, j.info.ModTime(), j.info.ModTime()); err != nil {
		return manifest.Entry{}, err
	}
	if err := os.Rename(partial, dst); err != nil {
		return manifest.Entry{}, err
	}
	ok = true

	return manifest.Entry{
		Path:       j.rel,
		Size:       n,
		SHA256:     hex.EncodeToString(h.Sum(nil)),
		MTime:      j.info.ModTime(),
		Source:     j.src,
		ArchivedAt: time.Now().UTC(),
	}, nil
}
