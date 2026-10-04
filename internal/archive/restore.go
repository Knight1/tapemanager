package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Knight1/tapemanager/internal/manifest"
	"github.com/Knight1/tapemanager/internal/parity"
)

// RestoreOptions configures Restore.
type RestoreOptions struct {
	TapeRoot string
	Path     string // file or directory on tape; empty means everything
	Dest     string // local directory to restore into
	Log      io.Writer
	Progress io.Writer
}

// RestoreResult reports what Restore did.
type RestoreResult struct {
	Files    int
	Repaired int // files rebuilt with parity
	Failed   int
	Skipped  int // references to other tapes
	Bytes    int64
	Duration time.Duration
}

// Restore copies files from the tape to a local directory, keeping their
// paths relative to the tape root. Read errors do not stop a file: damaged
// chunks are rebuilt with parity when possible. Every restored file is
// checked against its recorded SHA-256 before it is kept, so a restore
// never leaves wrong data behind. Existing files are never overwritten.
func Restore(opts RestoreOptions) (res RestoreResult, err error) {
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	tape, err := manifest.Open(opts.TapeRoot)
	if err != nil {
		return res, err
	}
	defer tape.Close()
	entries, err := tape.Entries()
	if err != nil {
		return res, err
	}
	chunks, err := tape.Chunks()
	if err != nil {
		return res, err
	}
	par, err := tape.Parity()
	if err != nil {
		return res, err
	}

	if err := os.MkdirAll(opts.Dest, 0o755); err != nil {
		return res, err
	}
	// All writes stay inside the destination, whatever the manifest says.
	dest, err := os.OpenRoot(opts.Dest)
	if err != nil {
		return res, err
	}
	defer dest.Close()

	prefix := strings.Trim(filepath.ToSlash(opts.Path), "/")
	matched := 0
	for _, e := range entries {
		if prefix != "" && e.Path != prefix && !strings.HasPrefix(e.Path, prefix+"/") {
			continue
		}
		matched++
		if e.Ref != nil {
			fmt.Fprintf(opts.Log, "SKIPPED:   %s\n       stored as %s on tape %s; mount that tape to restore it\n", e.Path, e.Ref.Path, e.Ref.Tape)
			res.Skipped++
			continue
		}
		fmt.Fprintf(opts.Log, "RESTORING: %s\n", e.Path)
		repaired, err := restoreFile(tape, dest, e, chunks, par, opts.Progress)
		if err != nil {
			fmt.Fprintf(opts.Log, "       FAILED: %v\n", err)
			res.Failed++
			continue
		}
		if repaired {
			fmt.Fprintf(opts.Log, "       REPAIRED with parity, SHA-256 OK\n")
			res.Repaired++
		} else {
			fmt.Fprintf(opts.Log, "       OK\n")
		}
		res.Files++
		res.Bytes += e.Size
	}
	if matched == 0 {
		return res, fmt.Errorf("nothing on tape matches %q", opts.Path)
	}
	return res, nil
}

func restoreFile(tape *manifest.Tape, dest *os.Root, e manifest.Entry, chunks map[string]manifest.Chunks, par map[string]manifest.Parity, prog io.Writer) (repaired bool, err error) {
	tf, err := openTapeFile(tape, e, chunks, par)
	if err != nil {
		return false, err
	}
	defer tf.Close()

	rel := filepath.FromSlash(e.Path)
	if _, err := dest.Lstat(rel); err == nil {
		return false, fmt.Errorf("%s already exists in the destination", e.Path)
	}
	if err := dest.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return false, err
	}
	tmp := rel + PartialSuffix
	out, err := dest.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			dest.Remove(tmp)
		}
	}()

	p := startProgress(prog, e.Size)
	s, err := tf.scan(out, p)
	p.finish()
	if err != nil {
		return false, err
	}
	if !s.intact(e) {
		detail := describeDamage(s, e, tf.chunkSize())
		if tf.parity == nil {
			return false, fmt.Errorf("%s; no parity to repair it", detail)
		}
		fixed, err := tf.repair(s)
		if err != nil {
			return false, fmt.Errorf("%s; parity cannot repair it: %v", detail, err)
		}
		for i, b := range fixed {
			off := i * tf.chunkSize()
			if tf.parity.Layout.Scheme == parity.SchemeSmall {
				off = 0
			}
			if _, err := out.WriteAt(b, off); err != nil {
				return false, err
			}
		}
		repaired = true
	}
	if err := out.Truncate(e.Size); err != nil {
		return false, err
	}

	// The restored file must match the archive exactly. Without repair the
	// scan already hashed exactly the bytes written; after a repair the
	// assembled file is read back and checked.
	if repaired {
		if _, err := out.Seek(0, io.SeekStart); err != nil {
			return false, err
		}
		h := sha256.New()
		if _, err := io.CopyBuffer(h, struct{ io.Reader }{out}, make([]byte, DefaultChunkSize)); err != nil {
			return false, err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
			return false, fmt.Errorf("restored data does not match the recorded SHA-256 (%s)", got)
		}
	}
	if err := out.Sync(); err != nil {
		return false, err
	}
	if err := out.Close(); err != nil {
		return false, err
	}
	ok = true
	if err := dest.Chtimes(tmp, e.MTime, e.MTime); err != nil {
		return false, err
	}
	// Never replace an existing file: a hard link fails if the name was
	// taken in the meantime, where a rename would silently overwrite.
	defer dest.Remove(tmp)
	if err := dest.Link(tmp, rel); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, fmt.Errorf("%s already exists in the destination", e.Path)
		}
		return false, err
	}
	return repaired, nil
}
