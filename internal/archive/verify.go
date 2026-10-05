package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/ltfs"
	"github.com/Knight1/tapemanager/internal/manifest"
	"github.com/Knight1/tapemanager/internal/memcheck"
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
	Problems   int // damaged metadata or parity data
	Bytes      int64
	Duration   time.Duration
}

// ErrNoEntries is returned when the manifest has nothing to verify.
var ErrNoEntries = errors.New("no manifest entries to verify")

// Verify reads every file listed in the tape manifest back from the tape,
// recomputes its SHA-256 and compares it with the recorded value. Files are
// read in manifest order, which matches the order they were written, to
// avoid unnecessary tape repositioning. A full verify also reads each
// segment's parity data right after that segment's files.
//
// Read errors do not stop the check: damage is reported as byte ranges, and
// files with parity are checked for whether they can be rebuilt. Damaged
// metadata is skipped and reported, so one bad spot does not hide the rest
// of the tape. A full verify that cannot finish is recorded as failed, so
// an older passing verification cannot keep allowing purges.
func Verify(opts VerifyOptions) (res VerifyResult, err error) {
	start := time.Now()
	prefix := strings.Trim(filepath.ToSlash(opts.Prefix), "/")
	full := prefix == ""

	// Manifest paths come from the tape. Its root refuses any path or
	// symlink that would lead outside it.
	tape, err := manifest.Open(opts.TapeRoot)
	if err != nil {
		return res, err
	}
	defer tape.Close()

	var entries []manifest.Entry
	vol, volErr := tape.Volume()
	if volErr != nil {
		fmt.Fprintf(opts.Log, "PROBLEM:   %v\n", volErr)
		res.Problems++
	}
	if opts.Catalog != nil && full && (vol != nil || volErr != nil) {
		defer func() {
			res.Duration = time.Since(start)
			// Too little memory on this machine says nothing about the
			// tape: nothing is recorded, an earlier result stands.
			if errors.Is(err, memcheck.ErrNoMemory) {
				return
			}
			if err != nil && !errors.Is(err, ErrNoEntries) {
				res.Failed++
			}
			// A damaged volume record must not leave an older pass in
			// place: find the tape another way and record the result.
			id := ""
			if vol != nil {
				id = vol.ID
			} else {
				id = opts.Catalog.FindTape(ltfs.VolumeUUID(opts.TapeRoot), entries)
			}
			if id == "" {
				err = errors.Join(err, fmt.Errorf("the tape's volume record is damaged and the tape could not be identified in the catalog; this result was NOT recorded"))
				return
			}
			if rerr := recordVerify(opts.Catalog, opts.TapeRoot, id, res); rerr != nil {
				err = errors.Join(err, fmt.Errorf("updating catalog: %w", rerr))
			}
		}()
		if vol != nil {
			if n, err := pendingCount(opts.Catalog.PendingPath(vol.ID)); err == nil && n > 0 {
				fmt.Fprintf(opts.Log, "WARNING:   %d archived files are not yet in the tape manifest; rerun 'archive put' to record them\n", n)
			}
		}
	}

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
		res.Problems++
	}

	var paritySegs map[int][]manifest.Parity
	if full {
		paritySegs = map[int][]manifest.Parity{}
		for _, p := range par {
			paritySegs[p.Segment] = append(paritySegs[p.Segment], p)
		}
	}
	checkParity := func(seg int) {
		list := paritySegs[seg]
		if len(list) == 0 {
			return
		}
		delete(paritySegs, seg)
		fmt.Fprintf(opts.Log, "PARITY:    segment %d\n", seg)
		if bad := verifyParity(tape, seg, list, opts.Progress); len(bad) > 0 {
			for _, b := range bad {
				fmt.Fprintf(opts.Log, "       DAMAGED: %s\n", b)
			}
			res.Problems += len(bad)
		} else {
			fmt.Fprintf(opts.Log, "       OK\n")
		}
	}

	seg := 0
	for _, e := range entries {
		if prefix != "" && e.Path != prefix && !strings.HasPrefix(e.Path, prefix+"/") {
			continue
		}
		if e.Segment != seg {
			checkParity(seg)
			seg = e.Segment
		}
		res.Files++
		if e.Ref != nil {
			fmt.Fprintf(opts.Log, "REFERENCE: %s\n       stored as %s on tape %s\n", e.Path, e.Ref.Path, e.Ref.Tape)
			res.Refs++
			continue
		}
		fmt.Fprintf(opts.Log, "VERIFYING: %s\n", e.Path)
		status, detail, ferr := verifyFile(tape, e, chunks, par, opts.Progress)
		if ferr != nil {
			// Not the tape's fault: stop, and never count it as damage.
			res.Duration = time.Since(start)
			return res, fmt.Errorf("verification stopped at %s: %w", e.Path, ferr)
		}
		switch status {
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
	checkParity(seg)
	// Parity of segments whose entries could not be read.
	for s := range paritySegs {
		checkParity(s)
	}

	res.Duration = time.Since(start)
	if res.Files == 0 && res.Problems == 0 {
		return res, ErrNoEntries
	}
	return res, nil
}

// verifyParity reads a segment's parity data from start to end and checks
// every parity shard against its hash. It returns a description of each
// file whose parity is damaged.
func verifyParity(tape *manifest.Tape, seg int, list []manifest.Parity, progOut io.Writer) []string {
	sort.Slice(list, func(i, j int) bool { return list[i].Offset < list[j].Offset })
	f, err := tape.OpenParity(seg)
	if err != nil {
		return []string{fmt.Sprintf("parity data of segment %d: %v", seg, err)}
	}
	defer f.Close()
	var total int64
	for _, p := range list {
		total += p.Layout.ParitySize()
	}
	prog := startProgress(progOut, total)
	defer prog.finish()

	var bad []string
	for _, p := range list {
		buf := make([]byte, p.Layout.ShardSize)
		damaged := 0
		for i, want := range p.Hashes {
			n, err := f.ReadAt(buf, p.Offset+int64(i)*p.Layout.ShardSize)
			prog.Write(buf)
			got := sha256.Sum256(buf[:n])
			if (err != nil && err != io.EOF) || int64(n) != p.Layout.ShardSize || hex.EncodeToString(got[:]) != want {
				damaged++
			}
		}
		if damaged > 0 {
			bad = append(bad, fmt.Sprintf("parity of %s: %d of %d pieces damaged", p.Path, damaged, len(p.Hashes)))
		}
	}
	return bad
}

func recordVerify(c *catalog.Catalog, tapeRoot, id string, res VerifyResult) error {
	if _, err := c.Import(tapeRoot); err != nil {
		// The tape manifest may be what is damaged. The verification
		// result must still be recorded against the known tape.
		if t, terr := c.Tape(id); terr != nil || t == nil {
			return err
		}
	}
	return c.RecordVerify(id, catalog.Verification{
		At:         time.Now().UTC(),
		Files:      res.Files,
		Verified:   res.Verified,
		Repairable: res.Repairable,
		Failed:     res.Failed,
		Problems:   res.Problems,
	})
}

type fileStatus int

const (
	statusOK fileStatus = iota
	statusRepairable
	statusFailed
)

// verifyFile checks one file. err is set only for problems of this machine
// (not enough memory), which say nothing about the tape.
func verifyFile(tape *manifest.Tape, e manifest.Entry, chunks map[string]manifest.Chunks, par map[string]manifest.Parity, prog io.Writer) (status fileStatus, detail string, err error) {
	tf, err := openTapeFile(tape, e, chunks, par)
	if err != nil {
		return statusFailed, err.Error(), nil
	}
	defer tf.Close()
	// Encrypted files are checked as stored; no key is needed.
	e = tf.entry
	if tf.size > e.Size {
		return statusFailed, fmt.Sprintf("file is larger than recorded (%d > %d bytes)", tf.size, e.Size), nil
	}

	p := startProgress(prog, e.Size)
	s, err := tf.scan(nil, p)
	p.finish()
	if errors.Is(err, memcheck.ErrNoMemory) {
		return statusFailed, "", err
	}
	if err != nil {
		return statusFailed, err.Error(), nil
	}
	if s.intact(e) {
		return statusOK, "", nil
	}
	detail = describeDamage(s, e, tf.chunkSize())
	if tf.parity == nil {
		return statusFailed, detail, nil
	}
	if err := tf.repair(s, nil); errors.Is(err, memcheck.ErrNoMemory) {
		return statusFailed, "", err
	} else if err != nil {
		return statusFailed, fmt.Sprintf("%s; parity cannot repair it: %v", detail, err), nil
	}
	return statusRepairable, detail, nil
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
