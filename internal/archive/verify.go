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
)

// VerifyOptions configures Verify.
type VerifyOptions struct {
	TapeRoot string
	Prefix   string    // only verify entries at or below this path; empty means all
	Log      io.Writer // per-file output
	Progress io.Writer // progress bar output, nil to disable
}

// VerifyResult reports what Verify found.
type VerifyResult struct {
	Files    int
	Verified int
	Failed   int
	Bytes    int64
	Duration time.Duration
}

// ErrNoEntries is returned when the manifest has nothing to verify.
var ErrNoEntries = errors.New("no manifest entries to verify")

// Verify reads every file listed in the tape manifest back from the tape,
// recomputes its SHA-256 and compares it with the recorded value. Files are
// read in manifest order, which matches the order they were written, to
// avoid unnecessary tape repositioning.
func Verify(opts VerifyOptions) (VerifyResult, error) {
	start := time.Now()
	var res VerifyResult

	entries, err := manifest.Load(opts.TapeRoot)
	if err != nil {
		return res, err
	}
	prefix := strings.Trim(filepath.ToSlash(opts.Prefix), "/")

	for _, e := range entries {
		if prefix != "" && e.Path != prefix && !strings.HasPrefix(e.Path, prefix+"/") {
			continue
		}
		res.Files++
		fmt.Fprintf(opts.Log, "VERIFYING: %s\n", e.Path)
		if err := verifyFile(opts.TapeRoot, e, opts.Progress); err != nil {
			fmt.Fprintf(opts.Log, "       FAILED: %v\n", err)
			res.Failed++
			continue
		}
		fmt.Fprintf(opts.Log, "       OK\n")
		res.Verified++
		res.Bytes += e.Size
	}

	res.Duration = time.Since(start)
	if res.Files == 0 {
		return res, ErrNoEntries
	}
	return res, nil
}

func verifyFile(tapeRoot string, e manifest.Entry, progressOut io.Writer) error {
	f, err := os.Open(filepath.Join(tapeRoot, filepath.FromSlash(e.Path)))
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	prog := startProgress(progressOut, e.Size)
	n, err := io.CopyBuffer(io.MultiWriter(h, prog), struct{ io.Reader }{f}, make([]byte, copyBufferSize))
	prog.finish()
	if err != nil {
		return err
	}
	if n != e.Size {
		return fmt.Errorf("size mismatch: manifest %d, tape %d", e.Size, n)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
		return fmt.Errorf("SHA-256 mismatch: manifest %s, tape %s", e.SHA256, got)
	}
	return nil
}
