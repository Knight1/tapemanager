package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"github.com/Knight1/tapemanager/internal/archive"
	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/drive"
)

// Drive access, replaceable in tests.
var (
	openDevice   = drive.Open
	resolveDrive = drive.Resolve
	listDrives   = drive.List
	mountPoints  = drive.MountPoints
	forMount     = drive.ForMount
	ltfsMounts   = drive.LTFSMounts
	// driveWarnings returns warnings about the drive holding the tape
	// mounted at tapeRoot, or nothing if that drive cannot be found.
	driveWarnings = func(tapeRoot string) []string {
		f, err := forMount(tapeRoot)
		if err != nil {
			return nil
		}
		d, err := openDevice(f.SG)
		if err != nil {
			return nil
		}
		defer d.Close()
		info, err := drive.Gather(d)
		if err != nil {
			return nil
		}
		return info.Warnings()
	}
)

// checkWriteProtect refuses to write when the drive holding the tape
// mounted at tapeRoot reports the cartridge write protected. If the drive
// cannot be found or asked, it returns nil: the read-only mount check in
// put and recover still applies.
var checkWriteProtect = driveWriteProtect

func driveWriteProtect(tapeRoot string) error {
	f, err := forMount(tapeRoot)
	if err != nil {
		return nil
	}
	d, err := openDevice(f.SG)
	if err != nil {
		return nil
	}
	defer d.Close()
	wp, err := drive.WriteProtected(d)
	if err != nil || !wp {
		return nil
	}
	return fmt.Errorf("the cartridge in %s is write protected; slide its write-protect tab back or insert another tape (a full WORM cartridge reports the same). Nothing was written", f.SG)
}

type driveFlags struct {
	*commonFlags
	device string
}

func newDriveFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *driveFlags) {
	fs, cf := newFlagSet(name, stderr)
	df := &driveFlags{commonFlags: cf}
	fs.StringVar(&df.device, "device", os.Getenv("TAPEMGR_DEVICE"), "drive device, for example /dev/sg2 (default: the drive of the mounted tape, or the only drive)")
	return fs, df
}

// open finds and opens the drive.
func (df *driveFlags) open() (drive.Device, drive.Found, error) {
	f, err := resolveDrive(df.device, df.tape)
	if err != nil {
		return nil, f, err
	}
	d, err := openDevice(f.SG)
	if err != nil {
		return nil, f, err
	}
	return d, f, nil
}

// printDriveWarnings shows drive problems after a tape operation, so read
// and write trouble or a cleaning request is noticed early.
func printDriveWarnings(tapeRoot string, stderr io.Writer) {
	for _, w := range driveWarnings(tapeRoot) {
		fmt.Fprintln(stderr, "DRIVE WARNING:", w)
	}
}

func cmdDriveList(args []string, stdout, stderr io.Writer) int {
	fs, _ := newDriveFlagSet("drive list", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	drives, err := listDrives()
	if err != nil {
		return fail(stderr, err)
	}
	if len(drives) == 0 {
		fmt.Fprintln(stdout, "No tape drives found.")
		return exitOK
	}
	for _, f := range drives {
		line := fmt.Sprintf("%-10s %s %s, firmware %s", f.SG, f.Vendor, f.Model, f.Rev)
		if len(f.Names) > 0 {
			line += " (" + strings.Join(f.Names, " ") + ")"
		}
		if mps, err := mountPoints(f, ""); err == nil && len(mps) > 0 {
			line += ", mounted at " + strings.Join(mps, " ")
		}
		fmt.Fprintln(stdout, line)
	}
	return exitOK
}

func cmdDriveInfo(args []string, stdout, stderr io.Writer) int {
	fs, df := newDriveFlagSet("drive info", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	d, _, err := df.open()
	if err != nil {
		return fail(stderr, err)
	}
	defer d.Close()
	info, err := drive.Gather(d)
	if err != nil {
		return fail(stderr, err)
	}
	printDriveInfo(stdout, info, catalogTapeFor(df.catalog, info))
	return exitOK
}

// catalogTapeFor names the catalog tape of the loaded cartridge, found by
// its LTFS volume UUID. It never creates the catalog.
func catalogTapeFor(dir string, info *drive.Info) string {
	if info.Cartridge == nil || info.Cartridge.LTFSVolume == "" {
		return ""
	}
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	cat, err := catalog.Open(dir)
	if err != nil {
		return ""
	}
	return cat.FindTape(info.Cartridge.LTFSVolume, nil)
}

func mib(v uint64) string {
	if v > math.MaxInt64>>20 {
		return fmt.Sprintf("%d MiB", v)
	}
	return archive.FormatBytes(int64(v) << 20)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printDriveInfo(w io.Writer, info *drive.Info, catalogTape string) {
	q := info.Inquiry
	fmt.Fprintf(w, "Drive:       %s %s, firmware %s, serial %s (%s)\n", q.Vendor, q.Product, q.Revision, orDash(info.Serial), info.Path)

	status := "ready"
	switch {
	case errors.Is(info.Ready, drive.ErrNoMedium):
		status = "no cartridge"
	case info.Ready != nil:
		status = "not ready: " + info.Ready.Error()
	}
	if v := info.VHF; v != nil {
		status += ", " + v.ActivityName()
		if v.WriteProtect {
			status += ", write protected"
		}
	}
	fmt.Fprintf(w, "Status:      %s\n", status)
	if v := info.VHF; v != nil {
		clean := "not needed"
		if v.CleanRequired {
			clean = "REQUIRED"
		} else if v.CleanRequested {
			clean = "REQUESTED"
		}
		fmt.Fprintf(w, "Cleaning:    %s\n", clean)
	}

	if c := info.Cartridge; c != nil {
		date := c.ManufactureDate
		if len(date) == 8 {
			date = date[:4] + "-" + date[4:6] + "-" + date[6:]
		}
		fmt.Fprintf(w, "Cartridge:   %s %s, serial %s, barcode %s, %s, made %s\n",
			orDash(c.Format), orDash(c.Kind), orDash(c.Serial), orDash(c.Barcode), orDash(c.Manufacturer), orDash(date))
		fmt.Fprintf(w, "Loads:       %d\n", c.LoadCount)
		for i, p := range c.Partitions {
			label := "Capacity:"
			if i > 0 {
				label = ""
			}
			fmt.Fprintf(w, "%-12s partition %d: %s free of %s\n", label, i, mib(p.RemainingMiB), mib(p.MaximumMiB))
		}
		fmt.Fprintf(w, "Lifetime:    %s written, %s read\n", mib(c.WrittenMiB), mib(c.ReadMiB))
		if c.Application != "" {
			fmt.Fprintf(w, "Formatted:   %s\n", c.Application)
		}
		if c.LTFSVolume != "" {
			line := c.LTFSVolume
			if catalogTape != "" {
				line += ", catalog tape " + catalogTape
			}
			fmt.Fprintf(w, "LTFS volume: %s\n", line)
		}
		if e := c.Errors; e != nil {
			fmt.Fprintf(w, "Tape errors: write %d retries, %d unrecovered; read %d retries, %d unrecovered (over its life)\n",
				e.WriteRetries, e.WriteUnrecovered, e.ReadRetries, e.ReadUnrecovered)
		}
	}
	counters := func(c *drive.ErrorCounters) string {
		if c == nil {
			return "-"
		}
		return fmt.Sprintf("%d corrected, %d uncorrected", c.Corrected, c.Uncorrected)
	}
	fmt.Fprintf(w, "Drive I/O:   write %s; read %s\n", counters(info.WriteErrors), counters(info.ReadErrors))

	if len(info.Alerts) == 0 {
		fmt.Fprintln(w, "TapeAlert:   none")
	}
	for i, a := range info.Alerts {
		label := "TapeAlert:"
		if i > 0 {
			label = ""
		}
		fmt.Fprintf(w, "%-12s %d (%s) %s\n", label, a.Flag, a.Severity, a.Name)
	}
	for _, p := range info.Problems {
		fmt.Fprintf(w, "PROBLEM:     %s\n", p)
	}
	for _, warn := range info.Warnings() {
		fmt.Fprintf(w, "WARNING:     %s\n", warn)
	}
}

func cmdDriveCheck(args []string, stdout, stderr io.Writer) int {
	fs, df := newDriveFlagSet("drive check", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	d, _, err := df.open()
	if err != nil {
		return fail(stderr, err)
	}
	defer d.Close()
	info, err := drive.Gather(d)
	if err != nil {
		return fail(stderr, err)
	}
	warnings := info.Warnings()
	for _, w := range warnings {
		fmt.Fprintln(stdout, "WARNING:", w)
	}
	for _, p := range info.Problems {
		fmt.Fprintln(stdout, "PROBLEM:", p)
	}
	if len(warnings) > 0 || len(info.Problems) > 0 {
		return exitFailure
	}
	fmt.Fprintf(stdout, "OK: %s %s (%s)\n", info.Inquiry.Vendor, info.Inquiry.Product, info.Path)
	return exitOK
}

func cmdDriveLoad(args []string, stdout, stderr io.Writer) int {
	fs, df := newDriveFlagSet("drive load", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	d, f, err := df.open()
	if err != nil {
		return fail(stderr, err)
	}
	defer d.Close()
	if err := requireTape(d); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Loading the cartridge in %s...\n", f.SG)
	if err := drive.Load(d); err != nil {
		if errors.Is(err, drive.ErrNoMedium) {
			return fail(stderr, errors.New("no cartridge in the drive; insert one first"))
		}
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "Loaded.")
	return exitOK
}

func cmdDriveEject(args []string, stdout, stderr io.Writer) int {
	fs, df := newDriveFlagSet("drive eject", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	d, f, err := df.open()
	if err != nil {
		return fail(stderr, err)
	}
	defer d.Close()
	if err := requireTape(d); err != nil {
		return fail(stderr, err)
	}
	serial, _ := drive.ReadSerial(d)
	// Ejecting under a mounted LTFS would lose its unwritten index. The
	// drive normally refuses too, because LTFS locks the cartridge, but a
	// crashed LTFS may leave the lock off while the mount remains.
	mps, err := mountPoints(f, serial)
	if err == nil && len(f.Names) == 0 {
		// A device unknown to sysfs cannot be matched to a mount, so any
		// LTFS mount might be on it.
		mps, err = ltfsMounts()
	}
	if err != nil {
		return fail(stderr, fmt.Errorf("checking mounts: %w", err))
	}
	if len(mps) > 0 {
		return fail(stderr, fmt.Errorf("the tape is mounted at %s; unmount it first so LTFS writes its index", strings.Join(mps, ", ")))
	}
	fmt.Fprintf(stdout, "Rewinding and ejecting the cartridge in %s...\n", f.SG)
	if err := drive.Unload(d); err != nil {
		switch {
		case errors.Is(err, drive.ErrNoMedium):
			return fail(stderr, drive.ErrNoMedium)
		case errors.Is(err, drive.ErrRemovalPrevented):
			return fail(stderr, drive.ErrRemovalPrevented)
		}
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "Ejected.")
	return exitOK
}

// requireTape refuses devices that are not tape drives before sending a
// command that moves media.
func requireTape(d drive.Device) error {
	q, err := drive.ReadInquiry(d)
	if err != nil {
		return err
	}
	if !q.IsTape() {
		return fmt.Errorf("%s is not a tape drive (%s %s)", d.Path(), q.Vendor, q.Product)
	}
	return nil
}
