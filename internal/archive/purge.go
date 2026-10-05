package archive

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Knight1/tapemanager/internal/catalog"
)

// PurgeOptions configures PlanPurge.
type PurgeOptions struct {
	Source   string // file or directory whose archived files should be deleted
	TapeRoot string // refuse to purge anything on or containing the tape
	Catalog  *catalog.Catalog
	Rehash   bool // reread each source file and compare its SHA-256
	Copies   int  // verified copies on different tapes required, 1 by default
}

// PurgeFile is a source file selected for deletion.
type PurgeFile struct {
	Path string // absolute
	Size int64
	Tape string // tapes holding the verified copies
	rel  string // relative to the plan root
	info fs.FileInfo
}

// PurgeKept is a source file that will not be deleted, and why.
type PurgeKept struct {
	Path   string
	Reason string
}

// PurgePlan lists what a purge would delete. Nothing is deleted until
// Execute is called.
type PurgePlan struct {
	Root   string // directory all paths are confined to
	Delete []PurgeFile
	Keep   []PurgeKept
	Bytes  int64
	Tapes  []string // tapes holding the verified copies
}

// PlanPurge decides which source files may be deleted. A file qualifies
// only if this machine archived it, it still has the same size and mtime,
// the tape manifest lists the same SHA-256, and the tape holding the content
// passed its most recent verification after the file was archived. With
// Copies above 1, that must hold on that many different tapes. Retired
// tapes never count. Deduplicated files qualify through the tape holding
// the content.
func PlanPurge(opts PurgeOptions) (*PurgePlan, error) {
	if opts.Copies <= 0 {
		opts.Copies = 1
	}
	if opts.Copies > MaxCopies {
		return nil, fmt.Errorf("at most %d copies are supported", MaxCopies)
	}
	src, err := filepath.Abs(opts.Source)
	if err != nil {
		return nil, err
	}
	if opts.TapeRoot != "" {
		tape, err := filepath.Abs(opts.TapeRoot)
		if err != nil {
			return nil, err
		}
		if within(src, tape) || within(tape, src) {
			return nil, fmt.Errorf("refusing to purge %s: it overlaps the tape at %s", src, tape)
		}
	}
	info, err := os.Lstat(src)
	if err != nil {
		return nil, err
	}

	plan := &PurgePlan{Root: src}
	var files []job
	switch {
	case info.Mode().IsRegular():
		plan.Root = filepath.Dir(src)
		files = []job{{src: src, rel: filepath.Base(src), info: info}}
	case info.IsDir():
		err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			files = append(files, job{src: p, rel: rel, info: fi})
			return nil
		})
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%s is not a regular file or directory", src)
	}

	// Where a source file went is taken only from records this machine
	// wrote. What the tape holds is taken from the cataloged tape manifest,
	// which is what verification checked.
	written, err := opts.Catalog.Written()
	if err != nil {
		return nil, err
	}
	bySource := make(map[string][]catalog.Hit)
	for _, h := range written {
		bySource[h.Entry.Source] = append(bySource[h.Entry.Source], h)
	}
	all, err := opts.Catalog.All()
	if err != nil {
		return nil, err
	}
	byLocation := make(map[string]catalog.Hit, len(all))
	for _, h := range all {
		byLocation[h.Tape.ID+"\x00"+h.Entry.Path] = h
	}

	tapes := map[string]bool{}
	for _, f := range files {
		found, sum, reason := purgeCheck(f, bySource[f.src], byLocation, opts.Copies)
		if reason == "" && opts.Rehash {
			reason = rehash(f, sum)
		}
		if reason != "" {
			plan.Keep = append(plan.Keep, PurgeKept{Path: f.src, Reason: reason})
			continue
		}
		plan.Delete = append(plan.Delete, PurgeFile{Path: f.src, Size: f.info.Size(), Tape: strings.Join(found, ", "), rel: f.rel, info: f.info})
		plan.Bytes += f.info.Size()
		for _, t := range found {
			tapes[t] = true
		}
	}
	for t := range tapes {
		plan.Tapes = append(plan.Tapes, t)
	}
	slices.Sort(plan.Tapes)
	return plan, nil
}

// purgeCheck returns the tapes and SHA-256 of enough verified copies of f,
// or the reason there are not enough. hits are this machine's records for f.
func purgeCheck(f job, hits []catalog.Hit, byLocation map[string]catalog.Hit, copies int) (tapes []string, sum, reason string) {
	if len(hits) == 0 {
		return nil, "", "not archived"
	}
	reason = "archived copy differs (size or mtime changed since archiving)"
	seen := map[string]bool{}
	for _, h := range hits {
		if h.Entry.Size != f.info.Size() || !h.Entry.MTime.Equal(f.info.ModTime()) {
			continue
		}
		// The tape manifest must hold the same content at the recorded
		// location, possibly through a deduplication reference.
		onTape, ok := byLocation[h.Tape.ID+"\x00"+h.Entry.Path]
		if !ok || onTape.Entry.SHA256 != h.Entry.SHA256 {
			reason = fmt.Sprintf("tape %s does not list this file; run 'catalog import' with the tape mounted", tapeLabel(h.Tape))
			continue
		}
		content := onTape
		if ref := onTape.Entry.Ref; ref != nil {
			target, ok := byLocation[ref.Tape+"\x00"+ref.Path]
			if !ok || target.Entry.Ref != nil || target.Entry.SHA256 != h.Entry.SHA256 {
				reason = "deduplicated copy not found in the catalog"
				continue
			}
			content = target
		}
		// All copies must be of the same content.
		if sum != "" && content.Entry.SHA256 != sum {
			continue
		}
		if seen[content.Tape.ID] {
			continue
		}
		if content.Tape.Retired != nil {
			reason = fmt.Sprintf("tape %s is retired", tapeLabel(content.Tape))
			continue
		}
		if !content.Tape.VerifiedSince(content.Entry.ArchivedAt) {
			reason = fmt.Sprintf("tape %s has not passed verification since archiving", tapeLabel(content.Tape))
			continue
		}
		seen[content.Tape.ID] = true
		sum = content.Entry.SHA256
		tapes = append(tapes, tapeLabel(content.Tape))
		if len(tapes) >= copies {
			slices.Sort(tapes)
			return tapes, sum, ""
		}
	}
	slices.Sort(tapes)
	if len(tapes) > 0 {
		return nil, "", fmt.Sprintf("only %d of %d required verified copies (%s)", len(tapes), copies, strings.Join(tapes, ", "))
	}
	return nil, "", reason
}

func rehash(f job, want string) string {
	in, err := openSource(f)
	if err != nil {
		return "cannot reread: " + err.Error()
	}
	defer in.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, struct{ io.Reader }{in}, make([]byte, DefaultChunkSize)); err != nil {
		return "cannot reread: " + err.Error()
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return "content differs from the archived copy"
	}
	return ""
}

func tapeLabel(t catalog.Tape) string {
	if t.Label != "" {
		return t.Label
	}
	return t.ID
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Execute deletes the planned files and then any directories left empty,
// never the plan root itself. Every file is checked again right before
// deletion and kept if it changed since planning. All access is confined to
// the plan root, so a directory swapped for a symlink cannot redirect a
// deletion elsewhere.
func (p *PurgePlan) Execute(log io.Writer) (deleted int, err error) {
	root, err := os.OpenRoot(p.Root)
	if err != nil {
		return 0, err
	}
	defer root.Close()

	dirs := map[string]bool{}
	for _, f := range p.Delete {
		st, err := root.Lstat(f.rel)
		if err != nil {
			fmt.Fprintf(log, "KEPT:      %s (%v)\n", f.Path, err)
			continue
		}
		if !st.Mode().IsRegular() || !os.SameFile(st, f.info) || st.Size() != f.info.Size() || !st.ModTime().Equal(f.info.ModTime()) {
			fmt.Fprintf(log, "KEPT:      %s (changed since the check)\n", f.Path)
			continue
		}
		if err := root.Remove(f.rel); err != nil {
			return deleted, err
		}
		fmt.Fprintf(log, "DELETED:   %s\n", f.Path)
		deleted++
		for d := filepath.Dir(f.rel); d != "."; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}

	// Deepest first, so parents empty out after their children.
	var list []string
	for d := range dirs {
		list = append(list, d)
	}
	slices.SortFunc(list, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, d := range list {
		if err := root.Remove(d); err != nil && !isNotEmpty(err) && !errors.Is(err, os.ErrNotExist) {
			return deleted, err
		}
	}
	return deleted, nil
}

func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
