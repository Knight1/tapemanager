// Command tapemgr manages LTO tapes used as archival storage via LTFS.
package main

import (
	"bufio"
	"errors"
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
  tapemgr archive purge-source [flags] <source>
                                           delete source files that are on a verified tape
  tapemgr archive recover [flags]          record files on tape that have no manifest entry
  tapemgr archive restore [flags] --to DIR [path]
                                           copy files from tape, repairing damage with parity
  tapemgr archive keygen --out FILE        create an age key pair for --encrypt-to
  tapemgr catalog import [flags]           copy the mounted tape's manifest into the catalog
  tapemgr catalog tapes [flags]            list known tapes
  tapemgr catalog search [flags] <query>   find files by path or SHA-256 prefix
  tapemgr catalog retire [--undo] <tape>   stop counting a lost or failing tape as a copy
  tapemgr drive list                       list attached tape drives
  tapemgr drive info [flags]               show drive, cartridge, error counters and TapeAlert flags
  tapemgr drive log [flags]                show and analyze the drive's error history
  tapemgr drive check [flags]              exit 1 if the drive needs cleaning or reports errors
  tapemgr drive load [flags]               load the inserted cartridge
  tapemgr drive eject [flags]              rewind and eject the cartridge (refused while mounted)
  tapemgr drive firmware --file IMAGE      update the drive's firmware (asks for confirmation)
  tapemgr drive keygen --out FILE          add a new key to an LTFS key file for drive encryption
  tapemgr version

Common flags:
  --tape DIR          LTFS mount point (default $TAPEMGR_TAPE or /mnt/ltfs)
  --catalog DIR       local catalog (default $TAPEMGR_CATALOG or /var/lib/tapemgr)
  --no-mount-check    allow a tape root that is not an LTFS mount
  --device PATH       drive commands: sg device (default $TAPEMGR_DEVICE, the
                      drive of the mounted tape, or the only drive)

Run 'tapemgr <group> <command> -h' for command flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmds := map[string]func([]string, io.Writer, io.Writer) int{
		"archive put":     cmdPut,
		"archive verify":  cmdVerify,
		"archive list":    cmdList,
		"archive recover": cmdRecover,
		"archive restore": cmdRestore,
		"catalog import":  cmdImport,
		"catalog tapes":   cmdTapes,
		"catalog search":  cmdSearch,
		"catalog retire":  cmdRetire,
		"archive keygen":  cmdArchiveKeygen,
		"drive list":      cmdDriveList,
		"drive info":      cmdDriveInfo,
		"drive check":     cmdDriveCheck,
		"drive log":       cmdDriveLog,
		"drive load":      cmdDriveLoad,
		"drive eject":     cmdDriveEject,
		"drive keygen":    cmdDriveKeygen,
	}
	if len(args) >= 2 {
		if args[0] == "archive" && args[1] == "purge-source" {
			return cmdPurge(args[2:], stdin, stdout, stderr)
		}
		if args[0] == "drive" && args[1] == "firmware" {
			return cmdDriveFirmware(args[2:], stdin, stdout, stderr)
		}
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

// envBool reports whether the environment variable is set to a true value.
func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
	parityPct := fs.Int("parity", 10, "Reed-Solomon parity overhead in percent (0 to 25, 0 disables)")
	copies := fs.Int("copies", 1, "number of different tapes each file should be on; 2 makes a second copy on this tape")
	again := fs.Bool("again", false, "archive files even if they already have enough copies on other tapes")
	requireEnc := fs.Bool("require-encryption", envBool("TAPEMGR_REQUIRE_ENCRYPTION"), "refuse to write unless the drive is encrypting (default $TAPEMGR_REQUIRE_ENCRYPTION)")
	var encryptTo, encryptToFile stringList
	fs.Var(&encryptTo, "encrypt-to", "encrypt each file with age to this public key (age1...); repeatable")
	fs.Var(&encryptToFile, "encrypt-to-file", "encrypt to the public keys in this file; repeatable (default $TAPEMGR_ENCRYPT_TO_FILE)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(encryptTo) == 0 && len(encryptToFile) == 0 && os.Getenv("TAPEMGR_ENCRYPT_TO_FILE") != "" {
		encryptToFile = append(encryptToFile, os.Getenv("TAPEMGR_ENCRYPT_TO_FILE"))
	}
	recipients, err := parseRecipients(encryptTo, encryptToFile)
	if err != nil {
		return fail(stderr, err)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: tapemgr archive put [flags] <source>")
		return exitUsage
	}
	if *copies < 1 {
		fmt.Fprintln(stderr, "tapemgr: --copies must be at least 1")
		return exitUsage
	}
	if err := cf.checkTape(); err != nil {
		return fail(stderr, err)
	}
	if err := checkWriteProtect(cf.tape); err != nil {
		return fail(stderr, err)
	}
	cat, err := catalog.Open(cf.catalog)
	if err != nil {
		return fail(stderr, err)
	}
	enc, err := checkEncryption(cat, cf.tape, *requireEnc, stdout)
	if err != nil {
		return fail(stderr, err)
	}

	sum, err := archive.Put(archive.PutOptions{
		TapeRoot:   cf.tape,
		Source:     fs.Arg(0),
		Prefix:     *prefix,
		Label:      *label,
		Catalog:    cat,
		Dedup:      !*noDedup,
		Parity:     *parityPct,
		Copies:     *copies,
		Again:      *again,
		Recipients: recipients,
		Log:        stdout,
		Progress:   progressOut(stderr),
	})
	if sum.Tape != nil {
		recordEncryption(cat, sum.Tape.ID, enc, stderr)
	}
	printDriveWarnings(cf.tape, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "tapemgr:", err)
		if errors.Is(err, archive.ErrTapeFull) {
			fmt.Fprintf(stderr, "Tape %s is full after %d files (%s). Files already archived will be skipped on the next tape.\n",
				tapeName(sum.Tape), sum.Files, archive.FormatBytes(sum.Bytes))
		} else {
			fmt.Fprintf(stderr, "Archive incomplete: %d files (%s) written before the error. Rerun the same command to continue.\n",
				sum.Files, archive.FormatBytes(sum.Bytes))
		}
		return exitFailure
	}
	fmt.Fprintf(stdout, "Archive complete.\nTape:        %s\nFiles:       %d\nSkipped:     %d already on this tape, %d on other tapes\nDuplicates:  %d\nBytes:       %s\nDuration:    %s\n",
		tapeName(sum.Tape), sum.Files, sum.Skipped, sum.Elsewhere, sum.Deduped, archive.FormatBytes(sum.Bytes), archive.FormatDuration(sum.Duration))
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
	printDriveWarnings(cf.tape, stderr)
	if err != nil && res.Files == 0 {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "\nFiles:       %d\nVerified:    %d\nRepairable:  %d\nFailed:      %d\nProblems:    %d\nReferences:  %d\nBytes:       %s\nDuration:    %s\n\n",
		res.Files, res.Verified, res.Repairable, res.Failed, res.Problems, res.Refs, archive.FormatBytes(res.Bytes), archive.FormatDuration(res.Duration))
	if err != nil {
		fmt.Fprintln(stderr, "tapemgr:", err)
	}
	if res.Failed > 0 {
		fmt.Fprintln(stdout, "SHA-256 verification FAILED.")
		return exitFailure
	}
	if res.Repairable > 0 {
		fmt.Fprintln(stdout, "The tape is damaged, but parity can rebuild every affected file. Restore them and copy them to another tape.")
		return exitFailure
	}
	if res.Problems > 0 {
		fmt.Fprintln(stdout, "Tape metadata or parity data is damaged. The files are intact, but their protection is not. Copy them to another tape.")
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
	if e.Recovered {
		ref = "  (recovered)"
	}
	if e.Age != nil {
		ref = "  (encrypted)"
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
			var issues []string
			if v.Failed > 0 {
				issues = append(issues, fmt.Sprintf("%d FAILED", v.Failed))
			}
			if v.Repairable > 0 {
				issues = append(issues, fmt.Sprintf("%d REPAIRABLE", v.Repairable))
			}
			if v.Problems > 0 {
				issues = append(issues, fmt.Sprintf("%d PROBLEMS", v.Problems))
			}
			status := "OK"
			if len(issues) > 0 {
				status = strings.Join(issues, ", ")
			}
			verified = fmt.Sprintf("verified %s %s", v.At.Format("2006-01-02"), status)
		}
		if t.Retired != nil {
			verified = "RETIRED " + t.Retired.Format("2006-01-02") + ", " + verified
		}
		if e := t.Encryption; e != nil {
			verified += ", encrypted"
			if len(e.Keys) > 0 {
				verified += " (key " + strings.Join(e.Keys, ", ") + ")"
			}
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
			name := tapeName(&h.Tape.Volume)
			if h.Tape.Retired != nil {
				name += " (retired)"
			}
			fmt.Fprintf(stdout, "%s\n", name)
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

func cmdPurge(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("archive purge-source", stderr)
	yes := fs.Bool("yes", false, "delete without asking for confirmation")
	rehash := fs.Bool("rehash", false, "reread every source file and compare its SHA-256 before deleting")
	copies := fs.Int("copies", 1, "verified copies on different tapes required before deleting")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: tapemgr archive purge-source [flags] <source>")
		return exitUsage
	}
	if *copies < 1 {
		fmt.Fprintln(stderr, "tapemgr: --copies must be at least 1")
		return exitUsage
	}
	cat, err := catalog.Open(cf.catalog)
	if err != nil {
		return fail(stderr, err)
	}
	plan, err := archive.PlanPurge(archive.PurgeOptions{
		Source:   fs.Arg(0),
		TapeRoot: cf.tape,
		Catalog:  cat,
		Rehash:   *rehash,
		Copies:   *copies,
	})
	if err != nil {
		return fail(stderr, err)
	}

	const maxListed = 20
	for i, k := range plan.Keep {
		if i == maxListed {
			fmt.Fprintf(stdout, "KEEP:      ... and %d more\n", len(plan.Keep)-maxListed)
			break
		}
		fmt.Fprintf(stdout, "KEEP:      %s (%s)\n", k.Path, k.Reason)
	}
	if len(plan.Keep) > 0 {
		fmt.Fprintln(stdout)
	}
	if len(plan.Delete) == 0 {
		fmt.Fprintln(stdout, "Nothing to delete.")
		return exitFailure
	}

	fmt.Fprintf(stdout, "This will delete %d files (%s) from:\n\n    %s\n\n", len(plan.Delete), archive.FormatBytes(plan.Bytes), plan.Root)
	if *copies > 1 {
		fmt.Fprintf(stdout, "Every file has %d verified copies on different tapes (%s).\n", *copies, strings.Join(plan.Tapes, ", "))
	} else {
		fmt.Fprintf(stdout, "The files have been verified on tape %s.\n", strings.Join(plan.Tapes, ", "))
	}
	if len(plan.Keep) > 0 {
		fmt.Fprintf(stdout, "%d files will be kept.\n", len(plan.Keep))
	}
	if !*yes {
		fmt.Fprint(stdout, "\nContinue? [y/N] ")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(stdout, "Aborted. Nothing was deleted.")
			return exitFailure
		}
	}
	fmt.Fprintln(stdout)

	deleted, err := plan.Execute(stdout)
	fmt.Fprintf(stdout, "\nDeleted %d of %d files.\n", deleted, len(plan.Delete))
	if err != nil {
		return fail(stderr, err)
	}
	if deleted != len(plan.Delete) || len(plan.Keep) > 0 {
		return exitFailure
	}
	return exitOK
}

func cmdRecover(args []string, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("archive recover", stderr)
	label := fs.String("label", "", "tape label, if the tape has never been used with tapemgr")
	requireEnc := fs.Bool("require-encryption", envBool("TAPEMGR_REQUIRE_ENCRYPTION"), "refuse to write unless the drive is encrypting (default $TAPEMGR_REQUIRE_ENCRYPTION)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: tapemgr archive recover [flags]")
		return exitUsage
	}
	if err := cf.checkTape(); err != nil {
		return fail(stderr, err)
	}
	if err := checkWriteProtect(cf.tape); err != nil {
		return fail(stderr, err)
	}
	cat, err := catalog.Open(cf.catalog)
	if err != nil {
		return fail(stderr, err)
	}
	enc, err := checkEncryption(cat, cf.tape, *requireEnc, stdout)
	if err != nil {
		return fail(stderr, err)
	}
	res, err := archive.Recover(archive.RecoverOptions{
		TapeRoot: cf.tape,
		Label:    *label,
		Catalog:  cat,
		Log:      stdout,
		Progress: progressOut(stderr),
	})
	if res.Tape != nil {
		recordEncryption(cat, res.Tape.ID, enc, stderr)
	}
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Recovered %d files (%s) on tape %s in %s.\n",
		res.Files, archive.FormatBytes(res.Bytes), tapeName(res.Tape), archive.FormatDuration(res.Duration))
	if res.Files > 0 {
		fmt.Fprintln(stdout, "Run 'tapemgr archive verify' before relying on them.")
	}
	return exitOK
}

func cmdRestore(args []string, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("archive restore", stderr)
	to := fs.String("to", "", "local directory to restore into (required)")
	var identities stringList
	fs.Var(&identities, "identity", "age private key file for encrypted files; repeatable (default $TAPEMGR_IDENTITY)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(identities) == 0 && os.Getenv("TAPEMGR_IDENTITY") != "" {
		identities = append(identities, os.Getenv("TAPEMGR_IDENTITY"))
	}
	ids, err := parseIdentities(identities)
	if err != nil {
		return fail(stderr, err)
	}
	if fs.NArg() > 1 || *to == "" {
		fmt.Fprintln(stderr, "usage: tapemgr archive restore [flags] --to DIR [path]")
		return exitUsage
	}
	if err := cf.checkTape(); err != nil {
		return fail(stderr, err)
	}
	opts := archive.RestoreOptions{
		TapeRoot:   cf.tape,
		Path:       fs.Arg(0),
		Dest:       *to,
		Log:        stdout,
		Progress:   progressOut(stderr),
		Identities: ids,
	}
	// The catalog only adds hints about other copies; restore works
	// without one, and a missing catalog is not created.
	if _, err := os.Stat(cf.catalog); err == nil {
		if cat, err := catalog.Open(cf.catalog); err == nil {
			opts.Catalog = cat
		}
	}
	res, err := archive.Restore(opts)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "\nRestored:    %d (%d repaired with parity)\nFailed:      %d\nSkipped:     %d\nBytes:       %s\nDuration:    %s\n",
		res.Files, res.Repaired, res.Failed, res.Skipped, archive.FormatBytes(res.Bytes), archive.FormatDuration(res.Duration))
	if res.Failed > 0 || res.Skipped > 0 {
		return exitFailure
	}
	return exitOK
}

func cmdRetire(args []string, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("catalog retire", stderr)
	undo := fs.Bool("undo", false, "count the tape as a copy again")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: tapemgr catalog retire [--undo] <tape label or ID>")
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
	var match []catalog.Tape
	for _, t := range tapes {
		if t.ID == fs.Arg(0) || t.Label == fs.Arg(0) {
			match = append(match, t)
		}
	}
	switch len(match) {
	case 0:
		return fail(stderr, fmt.Errorf("no tape %q in the catalog", fs.Arg(0)))
	case 1:
	default:
		return fail(stderr, fmt.Errorf("several tapes are labeled %q; use the tape ID", fs.Arg(0)))
	}
	t, err := cat.SetRetired(match[0].ID, !*undo)
	if err != nil {
		return fail(stderr, err)
	}
	if *undo {
		fmt.Fprintf(stdout, "Tape %s counts as a copy again.\n", tapeName(&t.Volume))
		return exitOK
	}
	fmt.Fprintf(stdout, "Tape %s is retired. Its files no longer count as copies: 'archive put' writes them to another tape again, and 'purge-source' does not rely on it.\n", tapeName(&t.Volume))
	return exitOK
}
