package archive

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/manifest"
)

// RestoreOptions configures Restore.
type RestoreOptions struct {
	TapeRoot string
	Path     string // file or directory on tape; empty means everything
	Dest     string // local directory to restore into
	Log      io.Writer
	Progress io.Writer
	// Catalog, if set, is used to name other tapes holding a copy of a
	// file that cannot be restored from this one.
	Catalog *catalog.Catalog
	// Identities are the age private keys used to decrypt encrypted files.
	Identities []age.Identity
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
	// Damaged metadata is skipped so the rest of the tape stays restorable.
	entries, problems, err := tape.EntriesLenient()
	if err != nil {
		return res, err
	}
	chunks, p2, err := tape.Chunks()
	if err != nil {
		return res, err
	}
	par, p3, err := tape.Parity()
	if err != nil {
		return res, err
	}
	for _, p := range append(append(problems, p2...), p3...) {
		fmt.Fprintf(opts.Log, "PROBLEM:   %s\n", p)
	}
	otherCopies := func(e manifest.Entry) {}
	if opts.Catalog != nil {
		var current string
		if v, err := tape.Volume(); err == nil && v != nil {
			current = v.ID
		}
		if cp, err := opts.Catalog.Copies(); err == nil {
			otherCopies = func(e manifest.Entry) {
				if tapes := cp.Tapes(e.SHA256, current); len(tapes) > 0 {
					fmt.Fprintf(opts.Log, "       another copy is on %s\n", tapeList(tapes))
				}
			}
		}
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
	noClobber := linkSupported(dest)
	if !noClobber {
		fmt.Fprintf(opts.Log, "NOTE:      %s does not support hard links; existing files are checked right before each file is moved into place\n", opts.Dest)
	}

	prefix := strings.Trim(filepath.ToSlash(opts.Path), "/")
	matched := 0
	for _, e := range entries {
		if prefix != "" && e.Path != prefix && !strings.HasPrefix(e.Path, prefix+"/") {
			continue
		}
		matched++
		if e.Ref != nil {
			fmt.Fprintf(opts.Log, "SKIPPED:   %s\n       stored as %s on tape %s; mount that tape to restore it\n", e.Path, e.Ref.Path, e.Ref.Tape)
			otherCopies(e)
			res.Skipped++
			continue
		}
		fmt.Fprintf(opts.Log, "RESTORING: %s\n", e.Path)
		repaired, err := restoreFile(tape, dest, noClobber, e, chunks, par, opts.Identities, opts.Progress)
		if err != nil {
			fmt.Fprintf(opts.Log, "       FAILED: %v\n", err)
			otherCopies(e)
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

// linkSupported reports whether the destination supports hard links, which
// restore uses to move files into place without ever overwriting. FAT and
// exFAT disks, and some network filesystems, do not.
func linkSupported(dest *os.Root) bool {
	probe := ".tapemgr-link-probe"
	dest.Remove(probe)
	dest.Remove(probe + "2")
	f, err := dest.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false
	}
	f.Close()
	defer dest.Remove(probe)
	if err := dest.Link(probe, probe+"2"); err != nil {
		return false
	}
	dest.Remove(probe + "2")
	return true
}

func restoreFile(tape *manifest.Tape, dest *os.Root, noClobber bool, e manifest.Entry, chunks map[string]manifest.Chunks, par map[string]manifest.Parity, ids []age.Identity, prog io.Writer) (repaired bool, err error) {
	if e.Age != nil && len(ids) == 0 {
		return false, fmt.Errorf("encrypted with age; pass --identity with the private key of one of: %s", strings.Join(e.Age.Recipients, ", "))
	}
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
	// A name unique to this attempt, created exclusively, so nothing that
	// already exists in the destination is ever touched.
	var rnd [8]byte
	rand.Read(rnd[:])
	tmp := rel + ".tapemgr-restore-" + hex.EncodeToString(rnd[:])
	out, err := dest.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	closed := false
	defer func() {
		if !closed {
			out.Close()
		}
		dest.Remove(tmp)
	}()

	// The file on tape is restored and checked as stored first; an
	// encrypted file is decrypted only once it is known to be intact.
	stored := tf.entry
	p := startProgress(prog, stored.Size)
	s, err := tf.scan(out, p)
	p.finish()
	if err != nil {
		return false, err
	}
	if !s.intact(stored) {
		detail := describeDamage(s, stored, tf.chunkSize())
		if tf.parity == nil {
			return false, fmt.Errorf("%s; no parity to repair it", detail)
		}
		err := tf.repair(s, func(off int64, b []byte) error {
			_, err := out.WriteAt(b, off)
			return err
		})
		if err != nil {
			return false, fmt.Errorf("%s; parity cannot repair it: %v", detail, err)
		}
		repaired = true
	}
	if err := out.Truncate(stored.Size); err != nil {
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
		if got := hex.EncodeToString(h.Sum(nil)); got != stored.SHA256 {
			return false, fmt.Errorf("restored data does not match the recorded SHA-256 (%s)", got)
		}
	}
	if e.Age != nil {
		plain, err := decryptRestored(dest, rel, out, stored.Size, e, ids)
		if err != nil {
			return false, err
		}
		closed = true
		out.Close()
		dest.Remove(tmp)
		tmp = plain
	} else {
		if err := out.Sync(); err != nil {
			return false, err
		}
		closed = true
		if err := out.Close(); err != nil {
			return false, err
		}
	}
	if err := dest.Chtimes(tmp, e.MTime, e.MTime); err != nil {
		return false, err
	}
	// Never replace an existing file. A hard link fails if the name was
	// taken in the meantime, where a rename would silently overwrite.
	// Without hard links, check right before the rename.
	if noClobber {
		if err := dest.Link(tmp, rel); err != nil {
			if errors.Is(err, os.ErrExist) {
				return false, fmt.Errorf("%s already exists in the destination", e.Path)
			}
			return false, err
		}
		return repaired, syncTapeDir(dest, filepath.Dir(rel))
	}
	if _, err := dest.Lstat(rel); err == nil {
		return false, fmt.Errorf("%s already exists in the destination", e.Path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := dest.Rename(tmp, rel); err != nil {
		return false, err
	}
	return repaired, syncTapeDir(dest, filepath.Dir(rel))
}

// decryptRestored decrypts the restored age file in src into a new
// temporary file next to rel and returns its name. The result is kept only
// if its size and SHA-256 match the original file.
func decryptRestored(dest *os.Root, rel string, src io.ReaderAt, size int64, e manifest.Entry, ids []age.Identity) (string, error) {
	r, err := age.Decrypt(io.NewSectionReader(src, 0, size), ids...)
	if err != nil {
		if _, ok := errors.AsType[*age.NoIdentityMatchError](err); ok {
			return "", fmt.Errorf("none of the given keys can decrypt it; it was encrypted to: %s", strings.Join(e.Age.Recipients, ", "))
		}
		return "", fmt.Errorf("decrypting: %w", err)
	}
	var rnd [8]byte
	rand.Read(rnd[:])
	name := rel + ".tapemgr-restore-" + hex.EncodeToString(rnd[:])
	f, err := dest.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			dest.Remove(name)
		}
	}()
	h := sha256.New()
	// One byte more than expected is read, so trailing data is noticed.
	n, err := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(r, e.Size+1), make([]byte, DefaultChunkSize))
	if err != nil {
		return "", fmt.Errorf("decrypting: %w", err)
	}
	if n != e.Size {
		return "", fmt.Errorf("decrypted size %d does not match the recorded %d", n, e.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
		return "", fmt.Errorf("decrypted data does not match the recorded SHA-256 (%s)", got)
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}
