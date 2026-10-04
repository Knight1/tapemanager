// Command tapemgr manages LTO tapes used as archival storage via LTFS.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Knight1/tapemanager/internal/archive"
	"github.com/Knight1/tapemanager/internal/ltfs"
	"github.com/Knight1/tapemanager/internal/manifest"
)

var version = "dev"

const usage = `tapemgr: LTFS tape archive manager

Usage:
  tapemgr archive put [flags] <source>     stream files onto the tape with SHA-256
  tapemgr archive verify [flags] [path]    read files back and check SHA-256
  tapemgr archive list [flags] [path]      list archived files from the manifest
  tapemgr version

Common flags:
  --tape DIR          LTFS mount point (default $TAPEMGR_TAPE or /mnt/ltfs)
  --no-mount-check    allow a tape root that is not an LTFS mount

Run 'tapemgr archive <command> -h' for command flags.
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
	switch args[0] {
	case "archive":
		if len(args) < 2 {
			fmt.Fprint(stderr, usage)
			return exitUsage
		}
		switch args[1] {
		case "put":
			return cmdPut(args[2:], stdout, stderr)
		case "verify":
			return cmdVerify(args[2:], stdout, stderr)
		case "list":
			return cmdList(args[2:], stdout, stderr)
		}
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

type tapeFlags struct {
	root         string
	noMountCheck bool
}

func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *tapeFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := &tapeFlags{}
	def := os.Getenv("TAPEMGR_TAPE")
	if def == "" {
		def = "/mnt/ltfs"
	}
	fs.StringVar(&tf.root, "tape", def, "LTFS mount point")
	fs.BoolVar(&tf.noMountCheck, "no-mount-check", false, "allow a tape root that is not an LTFS mount")
	return fs, tf
}

func (tf *tapeFlags) check() error {
	st, err := os.Stat(tf.root)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", tf.root)
	}
	if tf.noMountCheck {
		return nil
	}
	return ltfs.CheckMounted(tf.root)
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

func cmdPut(args []string, stdout, stderr io.Writer) int {
	fs, tf := newFlagSet("archive put", stderr)
	prefix := fs.String("dest", "", "destination directory on tape (default: source base name)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: tapemgr archive put [flags] <source>")
		return exitUsage
	}
	if err := tf.check(); err != nil {
		fmt.Fprintln(stderr, "tapemgr:", err)
		return exitFailure
	}

	sum, err := archive.Put(archive.PutOptions{
		TapeRoot: tf.root,
		Source:   fs.Arg(0),
		Prefix:   *prefix,
		Log:      stdout,
		Progress: progressOut(stderr),
	})
	if err != nil {
		fmt.Fprintln(stderr, "tapemgr:", err)
		fmt.Fprintf(stderr, "Archive incomplete: %d files (%s) written before the error. Rerun the same command to continue.\n",
			sum.Files, archive.FormatBytes(sum.Bytes))
		return exitFailure
	}
	fmt.Fprintf(stdout, "Archive complete.\nFiles:       %d\nSkipped:     %d\nBytes:       %s\nDuration:    %s\n",
		sum.Files, sum.Skipped, archive.FormatBytes(sum.Bytes), archive.FormatDuration(sum.Duration))
	return exitOK
}

func cmdVerify(args []string, stdout, stderr io.Writer) int {
	fs, tf := newFlagSet("archive verify", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: tapemgr archive verify [flags] [path]")
		return exitUsage
	}
	if err := tf.check(); err != nil {
		fmt.Fprintln(stderr, "tapemgr:", err)
		return exitFailure
	}

	res, err := archive.Verify(archive.VerifyOptions{
		TapeRoot: tf.root,
		Prefix:   fs.Arg(0),
		Log:      stdout,
		Progress: progressOut(stderr),
	})
	if err != nil {
		fmt.Fprintln(stderr, "tapemgr:", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "\nFiles:       %d\nVerified:    %d\nFailed:      %d\nBytes:       %s\nDuration:    %s\n\n",
		res.Files, res.Verified, res.Failed, archive.FormatBytes(res.Bytes), archive.FormatDuration(res.Duration))
	if res.Failed > 0 {
		fmt.Fprintln(stdout, "SHA-256 verification FAILED.")
		return exitFailure
	}
	fmt.Fprintln(stdout, "SHA-256 verification successful.")
	return exitOK
}

func cmdList(args []string, stdout, stderr io.Writer) int {
	fs, tf := newFlagSet("archive list", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: tapemgr archive list [flags] [path]")
		return exitUsage
	}
	if err := tf.check(); err != nil {
		fmt.Fprintln(stderr, "tapemgr:", err)
		return exitFailure
	}
	entries, err := manifest.Load(tf.root)
	if err != nil {
		fmt.Fprintln(stderr, "tapemgr:", err)
		return exitFailure
	}
	prefix := strings.Trim(fs.Arg(0), "/")
	var files int
	var bytes int64
	for _, e := range entries {
		if prefix != "" && e.Path != prefix && !strings.HasPrefix(e.Path, prefix+"/") {
			continue
		}
		fmt.Fprintf(stdout, "%s  %12d  %s\n", e.SHA256, e.Size, e.Path)
		files++
		bytes += e.Size
	}
	fmt.Fprintf(stdout, "\nFiles: %d  Bytes: %s\n", files, archive.FormatBytes(bytes))
	return exitOK
}
