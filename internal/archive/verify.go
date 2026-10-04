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

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/manifest"
)

// VerifyOptions configures Verify.
type VerifyOptions struct {
	TapeRoot string
	Prefix   string           // only verify entries at or below this path; empty means all
	Catalog  *catalog.Catalog // if set, the result is recorded in the catalog
	Log      io.Writer        // per-file output
	Progress io.Writer        // progress bar output, nil to disable
}

// VerifyResult reports what Verify found.
type VerifyResult struct {
	Files    int
	Verified int
	Failed   int
	Refs     int // deduplicated entries, whose content lives elsewhere
	Bytes    int64
	Duration time.Duration
}

// ErrNoEntries is returned when the manifest has nothing to verify.
var ErrNoEntries = errors.New("no manifest entries to verify")

// Verify reads every file listed in the tape manifest back from the tape,
// recomputes its SHA-256 and compares it with the recorded value. Files are
// read in manifest order, which matches the order they were written, to
// avoid unnecessary tape repositioning. When chunk hashes are available,
// damage is reported as byte ranges.
func Verify(opts VerifyOptions) (VerifyResult, error) {
	start := time.Now()
	var res VerifyResult

	// Manifest paths come from the tape. Its root refuses any path or
	// symlink that would lead outside it.
	tape, err := manifest.Open(opts.TapeRoot)
	if err != nil {
		return res, err
	}
	defer tape.Close()
	root := tape.Root()

	entries, err := tape.Entries()
	if err != nil {
		return res, err
	}
	chunks, err := tape.Chunks()
	if err != nil {
		return res, err
	}
	prefix := strings.Trim(filepath.ToSlash(opts.Prefix), "/")

	if opts.Catalog != nil {
		if vol, err := tape.Volume(); err == nil && vol != nil {
			if n, err := pendingCount(opts.Catalog.PendingPath(vol.ID)); err == nil && n > 0 {
				fmt.Fprintf(opts.Log, "WARNING:   %d archived files are not yet in the tape manifest; rerun 'archive put' to record them\n", n)
			}
		}
	}

	for _, e := range entries {
		if prefix != "" && e.Path != prefix && !strings.HasPrefix(e.Path, prefix+"/") {
			continue
		}
		res.Files++
		if e.Ref != nil {
			fmt.Fprintf(opts.Log, "REFERENCE: %s\n       stored as %s on tape %s\n", e.Path, e.Ref.Path, e.Ref.Tape)
			res.Refs++
			continue
		}
		fmt.Fprintf(opts.Log, "VERIFYING: %s\n", e.Path)
		var c *manifest.Chunks
		if v, ok := chunks[e.Path]; ok {
			c = &v
		}
		if err := verifyFile(root, e, c, opts.Progress); err != nil {
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
	if opts.Catalog != nil && prefix == "" {
		if err := recordVerify(opts.Catalog, opts.TapeRoot, res); err != nil {
			return res, fmt.Errorf("updating catalog: %w", err)
		}
	}
	return res, nil
}

func recordVerify(c *catalog.Catalog, tapeRoot string, res VerifyResult) error {
	t, err := c.Import(tapeRoot)
	if err != nil {
		return err
	}
	return c.RecordVerify(t.ID, catalog.Verification{
		At:       time.Now().UTC(),
		Files:    res.Files,
		Verified: res.Verified,
		Failed:   res.Failed,
	})
}

func verifyFile(root *os.Root, e manifest.Entry, c *manifest.Chunks, progressOut io.Writer) error {
	f, err := root.Open(filepath.FromSlash(e.Path))
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return err
	} else if !st.Mode().IsRegular() {
		return errors.New("not a regular file")
	}

	chunkSize := int64(DefaultChunkSize)
	if c != nil && c.ChunkSize > 0 {
		chunkSize = c.ChunkSize
	}

	h := sha256.New()
	prog := startProgress(progressOut, e.Size)
	defer prog.finish()

	buf := make([]byte, chunkSize)
	var n int64
	var bad []int
	for i := 0; ; i++ {
		m, rerr := io.ReadFull(f, buf)
		if m > 0 {
			h.Write(buf[:m])
			prog.Write(buf[:m])
			n += int64(m)
			if c != nil {
				sum := sha256.Sum256(buf[:m])
				if i >= len(c.SHA256) || hex.EncodeToString(sum[:]) != c.SHA256[i] {
					bad = append(bad, i)
				}
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read error at byte %d: %w", n, rerr)
		}
	}

	if n != e.Size {
		return fmt.Errorf("size mismatch: manifest %d, tape %d", e.Size, n)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
		if len(bad) > 0 {
			return fmt.Errorf("SHA-256 mismatch, damaged bytes: %s", badRanges(bad, chunkSize, e.Size))
		}
		return fmt.Errorf("SHA-256 mismatch: manifest %s, tape %s", e.SHA256, got)
	}
	return nil
}

// badRanges merges consecutive bad chunk indexes into byte ranges.
func badRanges(bad []int, chunkSize, size int64) string {
	var parts []string
	for i := 0; i < len(bad); {
		j := i
		for j+1 < len(bad) && bad[j+1] == bad[j]+1 {
			j++
		}
		from := int64(bad[i]) * chunkSize
		to := min(int64(bad[j]+1)*chunkSize, size)
		parts = append(parts, fmt.Sprintf("%d-%d", from, to-1))
		i = j + 1
	}
	return strings.Join(parts, ", ")
}
