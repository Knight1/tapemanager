package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	Files      int
	Verified   int
	Repairable int // damaged, but parity can rebuild them
	Failed     int // damaged beyond repair, or unreadable
	Refs       int // deduplicated entries, whose content lives elsewhere
	Bytes      int64
	Duration   time.Duration
}

// ErrNoEntries is returned when the manifest has nothing to verify.
var ErrNoEntries = errors.New("no manifest entries to verify")

// Verify reads every file listed in the tape manifest back from the tape,
// recomputes its SHA-256 and compares it with the recorded value. Files are
// read in manifest order, which matches the order they were written, to
// avoid unnecessary tape repositioning. Read errors do not stop the check:
// damage is reported as byte ranges, and files with parity are checked for
// whether they can be rebuilt.
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
		switch status, detail := verifyFile(tape, e, chunks, par, opts.Progress); status {
		case statusOK:
			fmt.Fprintf(opts.Log, "       OK\n")
			res.Verified++
			res.Bytes += e.Size
		case statusRepairable:
			fmt.Fprintf(opts.Log, "       DAMAGED: %s\n       repairable with parity: restore it with 'tapemgr archive restore'\n", detail)
			res.Repairable++
		default:
			fmt.Fprintf(opts.Log, "       FAILED: %s\n", detail)
			res.Failed++
		}
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
		At:         time.Now().UTC(),
		Files:      res.Files,
		Verified:   res.Verified,
		Repairable: res.Repairable,
		Failed:     res.Failed,
	})
}

type fileStatus int

const (
	statusOK fileStatus = iota
	statusRepairable
	statusFailed
)

func verifyFile(tape *manifest.Tape, e manifest.Entry, chunks map[string]manifest.Chunks, par map[string]manifest.Parity, prog io.Writer) (fileStatus, string) {
	tf, err := openTapeFile(tape, e, chunks, par)
	if err != nil {
		return statusFailed, err.Error()
	}
	defer tf.Close()
	if st, err := tf.f.Stat(); err == nil && st.Size() > e.Size {
		return statusFailed, fmt.Sprintf("file is larger than recorded (%d > %d bytes)", st.Size(), e.Size)
	}

	p := startProgress(prog, e.Size)
	s, err := tf.scan(nil, p)
	p.finish()
	if err != nil {
		return statusFailed, err.Error()
	}
	if s.intact(e) {
		return statusOK, ""
	}
	detail := describeDamage(s, e, tf.chunkSize())
	if tf.parity == nil {
		return statusFailed, detail
	}
	if _, err := tf.repair(s); err != nil {
		return statusFailed, fmt.Sprintf("%s; parity cannot repair it: %v", detail, err)
	}
	return statusRepairable, detail
}

// hashStream reads r to the end in chunkSize pieces and returns the SHA-256
// of everything, the SHA-256 of each piece, and the byte count.
func hashStream(r io.Reader, chunkSize int64, prog io.Writer) (sum string, chunks []string, n int64, err error) {
	h := sha256.New()
	buf := make([]byte, chunkSize)
	for {
		m, rerr := io.ReadFull(r, buf)
		if m > 0 {
			h.Write(buf[:m])
			prog.Write(buf[:m])
			c := sha256.Sum256(buf[:m])
			chunks = append(chunks, hex.EncodeToString(c[:]))
			n += int64(m)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return hex.EncodeToString(h.Sum(nil)), chunks, n, nil
		}
		if rerr != nil {
			return "", nil, n, fmt.Errorf("read error at byte %d: %w", n, rerr)
		}
	}
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
