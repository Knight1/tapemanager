// Command tapemgr manages LTO tapes used as archival storage via LTFS.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Knight1/tapemanager/internal/archive"
	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/ltfs"
	"github.com/Knight1/tapemanager/internal/manifest"
)

var version = "dev"

const usage = `tapemgr: LTFS tape archive manager

Usage:
  tapemgr archive put [flags] <source>     stream files onto the tape with SHA-256
  tapemgr archive verify [flags] [path]    read files back and check SHA-256
  tapemgr archive list [flags] [path]      list archived files from the tape manifest
  tapemgr catalog import [flags]           copy the mounted tape's manifest into the catalog
  tapemgr catalog tapes [flags]            list known tapes
  tapemgr catalog search [flags] <query>   find files by path or SHA-256 prefix
  tapemgr version

Common flags:
  --tape DIR          LTFS mount point (default $TAPEMGR_TAPE or /mnt/ltfs)
  --catalog DIR       local catalog (default $TAPEMGR_CATALOG or /var/lib/tapemgr)
  --no-mount-check    allow a tape root that is not an LTFS mount

Run 'tapemgr <group> <command> -h' for command flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmds := map[string]func([]string, io.Writer, io.Writer) int{
		"archive put":    cmdPut,
		"archive verify": cmdVerify,
		"archive list":   cmdList,
		"catalog import": cmdImport,
		"catalog tapes":  cmdTapes,
		"catalog search": cmdSearch,
	}
	if len(args) >= 2 {
		if fn, ok := cmds[args[0]+" "+args[1]]; ok {
			return fn(args[2:], stdout, stderr)
		}
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, "tapemgr", version)
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "tapemgr: unknown command %q\n\n%s", strings.Join(args, " "), usage)
	return exitUsage
}

type commonFlags struct {
	tape         string
	catalog      string
	noMountCheck bool
}

func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *commonFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := &commonFlags{}
	fs.StringVar(&cf.tape, "tape", envOr("TAPEMGR_TAPE", "/mnt/ltfs"), "LTFS mount point")
	fs.StringVar(&cf.catalog, "catalog", envOr("TAPEMGR_CATALOG", "/var/lib/tapemgr"), "local catalog directory")
	fs.BoolVar(&cf.noMountCheck, "no-mount-check", false, "allow a tape root that is not an LTFS mount")
	return fs, cf
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (cf *commonFlags) checkTape() error {
	st, err := os.Stat(cf.tape)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", cf.tape)
	}
	if cf.noMountCheck {
		return nil
	}
	return ltfs.CheckMounted(cf.tape)
}

// progressOut returns stderr when it is a terminal, so progress bars do not
// end up in logs or pipes.
func progressOut(stderr io.Writer) io.Writer {
	f, ok := stderr.(*os.File)
	if !ok {
		return nil
	}
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	return stderr
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "tapemgr:", err)
	return exitFailure
}

func cmdPut(args []string, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("archive put", stderr)
	prefix := fs.String("dest", "", "destination directory on tape (default: source base name)")
	label := fs.String("label", "", "tape label, set when the tape is first used")
	noDedup := fs.Bool("no-dedup", false, "always write content, even if an identical file is already archived")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: tapemgr archive put [flags] <source>")
		return exitUsage
	}
	if err := cf.checkTape(); err != nil {
		return fail(stderr, err)
	}
	cat, err := catalog.Open(cf.catalog)
	if err != nil {
		return fail(stderr, err)
	}

	sum, err := archive.Put(archive.PutOptions{
		TapeRoot: cf.tape,
		Source:   fs.Arg(0),
		Prefix:   *prefix,
		Label:    *label,
		Catalog:  cat,
		Dedup:    !*noDedup,
		Log:      stdout,
		Progress: progressOut(stderr),
	})
	if err != nil {
		fmt.Fprintln(stderr, "tapemgr:", err)
		fmt.Fprintf(stderr, "Archive incomplete: %d files (%s) written before the error. Rerun the same command to continue.\n",
			sum.Files, archive.FormatBytes(sum.Bytes))
		return exitFailure
	}
	fmt.Fprintf(stdout, "Archive complete.\nTape:        %s\nFiles:       %d\nSkipped:     %d\nDuplicates:  %d\nBytes:       %s\nDuration:    %s\n",
		tapeName(sum.Tape), sum.Files, sum.Skipped, sum.Deduped, archive.FormatBytes(sum.Bytes), archive.FormatDuration(sum.Duration))
	return exitOK
}

func tapeName(v *manifest.Volume) string {
	if v == nil {
		return "?"
	}
	if v.Label != "" {
		return v.Label + " (" + v.ID + ")"
	}
	return v.ID
}

func cmdVerify(args []string, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("archive verify", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: tapemgr archive verify [flags] [path]")
		return exitUsage
	}
	if err := cf.checkTape(); err != nil {
		return fail(stderr, err)
	}
	cat, err := catalog.Open(cf.catalog)
	if err != nil {
		return fail(stderr, err)
	}

	res, err := archive.Verify(archive.VerifyOptions{
		TapeRoot: cf.tape,
		Prefix:   fs.Arg(0),
		Catalog:  cat,
		Log:      stdout,
		Progress: progressOut(stderr),
	})
	if err != nil && res.Files == 0 {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "\nFiles:       %d\nVerified:    %d\nFailed:      %d\nReferences:  %d\nBytes:       %s\nDuration:    %s\n\n",
		res.Files, res.Verified, res.Failed, res.Refs, archive.FormatBytes(res.Bytes), archive.FormatDuration(res.Duration))
	if err != nil {
		fmt.Fprintln(stderr, "tapemgr:", err)
	}
	if res.Failed > 0 {
		fmt.Fprintln(stdout, "SHA-256 verification FAILED.")
		return exitFailure
	}
	fmt.Fprintln(stdout, "SHA-256 verification successful.")
	if err != nil {
		return exitFailure
	}
	return exitOK
}

func cmdList(args []string, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("archive list", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: tapemgr archive list [flags] [path]")
		return exitUsage
	}
	if err := cf.checkTape(); err != nil {
		return fail(stderr, err)
	}
	tape, err := manifest.Open(cf.tape)
	if err != nil {
		return fail(stderr, err)
	}
	defer tape.Close()
	entries, err := tape.Entries()
	if err != nil {
		return fail(stderr, err)
	}
	prefix := strings.Trim(fs.Arg(0), "/")
	var files int
	var bytes int64
	for _, e := range entries {
		if prefix != "" && e.Path != prefix && !strings.HasPrefix(e.Path, prefix+"/") {
			continue
		}
		printEntry(stdout, e)
		files++
		bytes += e.Size
	}
	fmt.Fprintf(stdout, "\nFiles: %d  Bytes: %s\n", files, archive.FormatBytes(bytes))
	return exitOK
}

func printEntry(w io.Writer, e manifest.Entry) {
	ref := ""
	if e.Ref != nil {
		ref = fmt.Sprintf("  -> %s:%s", e.Ref.Tape, e.Ref.Path)
	}
	fmt.Fprintf(w, "%s  %12d  %s%s\n", e.SHA256, e.Size, e.Path, ref)
}

func cmdImport(args []string, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("catalog import", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := cf.checkTape(); err != nil {
		return fail(stderr, err)
	}
	cat, err := catalog.Open(cf.catalog)
	if err != nil {
		return fail(stderr, err)
	}
	t, err := cat.Import(cf.tape)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Imported %s: %d files, %s\n", tapeName(&t.Volume), t.Files, archive.FormatBytes(t.Bytes))
	return exitOK
}

func cmdTapes(args []string, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("catalog tapes", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cat, err := catalog.Open(cf.catalog)
	if err != nil {
		return fail(stderr, err)
	}
	tapes, err := cat.Tapes()
	if err != nil {
		return fail(stderr, err)
	}
	for _, t := range tapes {
		verified := "never verified"
		if v := t.LastVerified(); v != nil {
			status := "OK"
			if v.Failed > 0 {
				status = fmt.Sprintf("%d FAILED", v.Failed)
			}
			verified = fmt.Sprintf("verified %s %s", v.At.Format("2006-01-02"), status)
		}
		fmt.Fprintf(stdout, "%-16s  %s  %8d files  %10s  %s\n",
			t.Label, t.ID, t.Files, archive.FormatBytes(t.Bytes), verified)
	}
	return exitOK
}

func cmdSearch(args []string, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("catalog search", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: tapemgr catalog search [flags] <query>")
		return exitUsage
	}
	cat, err := catalog.Open(cf.catalog)
	if err != nil {
		return fail(stderr, err)
	}
	hits, err := cat.Search(fs.Arg(0))
	if err != nil {
		return fail(stderr, err)
	}
	last := ""
	for _, h := range hits {
		if h.Tape.ID != last {
			fmt.Fprintf(stdout, "%s\n", tapeName(&h.Tape.Volume))
			last = h.Tape.ID
		}
		fmt.Fprint(stdout, "  ")
		printEntry(stdout, h.Entry)
	}
	if len(hits) == 0 {
		fmt.Fprintln(stdout, "No matches.")
		return exitFailure
	}
	return exitOK
}
