package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/Knight1/tapemanager/internal/archive"
	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/drive"
)

// How long to wait for a self-test to finish; tests shorten it. The command
// returns as soon as the test completes, so this is only a safety cap.
var (
	selfTestWait     = 90 * time.Minute
	selfTestInterval = 5 * time.Second
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

// line prints one labeled line of a section.
func line(w io.Writer, label, format string, args ...any) {
	if label != "" {
		label += ":"
	}
	fmt.Fprintf(w, "  %-14s %s\n", label, fmt.Sprintf(format, args...))
}

// num formats a counter the drive may not report.
func num(c drive.Count) string {
	if !c.OK {
		return "-"
	}
	return fmt.Sprint(c.N)
}

func plural(c drive.Count, one, many string) string {
	if c.OK && c.N == 1 {
		return "1 " + one
	}
	return num(c) + " " + many
}

func bytesOf(c drive.Count) string {
	if !c.OK {
		return "-"
	}
	if c.N > math.MaxInt64 {
		return fmt.Sprintf("%d bytes", c.N)
	}
	return archive.FormatBytes(int64(c.N))
}

func ratio(c drive.Count) string {
	if !c.OK {
		return "-"
	}
	return fmt.Sprintf("%d.%02d:1", c.N/100, c.N%100)
}

// printDriveInfo shows the drive first and the loaded tape second, so
// what belongs to the drive and what to the cartridge never mixes.
func printDriveInfo(w io.Writer, info *drive.Info, catalogTape string) {
	q := info.Inquiry
	fmt.Fprintln(w, "Drive")
	line(w, "Model", "%s %s, firmware %s, serial %s (%s)", q.Vendor, q.Product, q.Revision, orDash(info.Serial), info.Path)
	if fb := info.Firmware; fb.Name != "" {
		build := fb.Name
		if fb.Built != "" {
			build += ", built " + fb.Built
		}
		if fb.Platform != "" {
			build += ", " + fb.Platform
		}
		line(w, "Firmware", "%s", build)
	}

	status := "ready"
	switch {
	case errors.Is(info.Ready, drive.ErrNoMedium):
		status = "no cartridge"
	case errors.Is(info.Ready, drive.ErrNotLoaded):
		status = "cartridge ejected, still in the slot"
	case info.Ready != nil:
		status = "not ready: " + info.Ready.Error()
	}
	if v := info.VHF; v != nil {
		status += ", " + v.ActivityName()
	}
	line(w, "Status", "%s", status)

	clean := ""
	if v := info.VHF; v != nil {
		clean = "not needed"
		if v.CleanRequired || info.CleaningRequired {
			clean = "REQUIRED"
		} else if v.CleanRequested {
			clean = "REQUESTED"
		}
	}
	if st := info.Stats; st != nil {
		if clean != "" {
			clean += "; "
		}
		clean += plural(st.Cleanings, "cleaning", "cleanings") + " so far"
		if st.HoursSinceCleaning.OK {
			clean += fmt.Sprintf(", %d tape hours since the last one", st.HoursSinceCleaning.N)
		}
	}
	if clean != "" {
		line(w, "Cleaning", "%s", clean)
	}

	if e := info.Encryption; e != nil {
		enc := "off"
		if e.Encrypting() {
			enc = "on"
			if alg := info.EncryptionAlgorithm(); alg != "" {
				enc += ", " + alg
			}
			if k := e.KeyName(); k != "" {
				enc += ", key " + k
			}
		} else if e.Decrypting() {
			enc = "decrypting only"
		}
		if !e.Encrypting() {
			for _, a := range info.Algorithms {
				if a.Usable {
					enc += fmt.Sprintf(" (supports %s for the loaded cartridge)", a.Name())
					break
				}
			}
		}
		line(w, "Encryption", "%s", enc)
	}
	if c := info.Compression; c != nil {
		comp := "off"
		if !c.Enabled.OK {
			comp = "-"
		} else if c.Enabled.N != 0 {
			comp = "on"
		}
		line(w, "Compression", "%s; since the cartridge was loaded: written %s (%s to %s), read %s (%s to %s)", comp,
			ratio(c.WriteRatio), bytesOf(c.FromHost), bytesOf(c.ToTape), ratio(c.ReadRatio), bytesOf(c.FromTape), bytesOf(c.ToHost))
	}
	if st := info.Stats; st != nil {
		years := ""
		if st.PowerOnHours.OK {
			years = fmt.Sprintf(" (%.1f years)", float64(st.PowerOnHours.N)/8766)
		}
		line(w, "Powered on", "%s hours%s, %s", num(st.PowerOnHours), years, plural(st.PowerCycles, "power cycle", "power cycles"))
		km := "-"
		if st.MetersOfTape.OK {
			km = fmt.Sprint(st.MetersOfTape.N / 1000)
		}
		line(w, "Lifetime use", "%s cartridge loads, %s hours of tape motion, %s km of tape", num(st.Loads), num(st.HeadHours), km)
		var byFormat []string
		for _, f := range st.HeadHoursByFormat {
			byFormat = append(byFormat, fmt.Sprintf("%s %d h", f.Format, f.Hours))
		}
		if len(byFormat) > 0 {
			line(w, "", "tape motion by cartridge type: %s", strings.Join(byFormat, ", "))
		}
	}
	counters := func(c *drive.ErrorCounters) string {
		if c == nil {
			return "-"
		}
		return fmt.Sprintf("%d corrected, %d uncorrected", c.Corrected, c.Uncorrected)
	}
	line(w, "Errors", "write %s; read %s (current counters)", counters(info.WriteErrors), counters(info.ReadErrors))
	if st := info.Stats; st != nil {
		line(w, "", "lifetime: %s, %s; %s", plural(st.HardWriteErrors, "hard write error", "hard write errors"),
			plural(st.HardReadErrors, "hard read error", "hard read errors"), plural(info.NonMediumErrors, "non-medium error", "non-medium errors"))
	}
	if info.ErrorLogSlots > 0 {
		line(w, "Error log", "%d of %d slots used%s", len(info.ErrorLog), info.ErrorLogSlots,
			map[bool]string{true: "; details: tapemgr drive log", false: ""}[len(info.ErrorLog) > 0])
	}
	if len(info.Alerts) == 0 {
		line(w, "TapeAlert", "none")
	}
	for i, a := range info.Alerts {
		label := "TapeAlert"
		if i > 0 {
			label = ""
		}
		line(w, label, "%d (%s) %s", a.Flag, a.Severity, a.Name)
	}

	fmt.Fprintln(w, "\nTape")
	c := info.Cartridge
	switch {
	case c == nil && errors.Is(info.Ready, drive.ErrNotLoaded):
		line(w, "", "cartridge ejected but still in the slot; take it out, or load it again ('tapemgr drive load')")
	case c == nil:
		line(w, "", "no cartridge loaded")
	default:
		date := c.ManufactureDate
		if len(date) == 8 {
			date = date[:4] + "-" + date[4:6] + "-" + date[6:]
		}
		line(w, "Cartridge", "%s %s, serial %s, barcode %s, %s, made %s",
			orDash(c.Format), orDash(c.Kind), orDash(c.Serial), orDash(c.Barcode), orDash(c.Manufacturer), orDash(date))
		wp := "no"
		if c.WriteProtected {
			wp = "YES"
		}
		line(w, "Write protect", "%s", wp)
		if c.LTFSVolume != "" {
			vol := c.LTFSVolume
			if catalogTape != "" {
				vol += ", catalog tape " + catalogTape
			}
			line(w, "LTFS volume", "%s", vol)
		}
		if c.Application != "" {
			line(w, "Formatted by", "%s", c.Application)
		}
		for i, p := range c.Partitions {
			label := "Capacity"
			if i > 0 {
				label = ""
			}
			line(w, label, "partition %d: %s free of %s", i, mib(p.RemainingMiB), mib(p.MaximumMiB))
		}
		vs := c.Stats
		if vs != nil && vs.NativeCapacityMB.OK && vs.UsedNativeMB.OK {
			mb := func(n uint64) drive.Count {
				if n > math.MaxUint64/1_000_000 {
					return drive.Count{N: math.MaxUint64, OK: true}
				}
				return drive.Count{N: n * 1_000_000, OK: true}
			}
			line(w, "Used", "%s of %s native, before drive compression", bytesOf(mb(vs.UsedNativeMB.N)), bytesOf(mb(vs.NativeCapacityMB.N)))
		}
		usage := fmt.Sprintf("%d loads", c.LoadCount)
		if vs != nil {
			usage += ", " + plural(vs.Mounts, "mount", "mounts") + ", " + plural(vs.Passes, "full pass", "full passes")
		}
		line(w, "Lifetime use", "%s", usage)
		last := func(mb drive.Count) string {
			if vs == nil || !mb.OK {
				return ""
			}
			return fmt.Sprintf(", %d MB during the last mount", mb.N)
		}
		var lw, lr drive.Count
		if vs != nil {
			lw, lr = vs.LastMountMBWritten, vs.LastMountMBRead
		}
		line(w, "Written", "%s over its life%s", mib(c.WrittenMiB), last(lw))
		line(w, "Read", "%s over its life%s", mib(c.ReadMiB), last(lr))
		if vs != nil {
			line(w, "Errors", "write %s retries, %s unrecovered; read %s retries, %s unrecovered (over its life)",
				num(vs.WriteRetries), num(vs.WriteUnrecovered), num(vs.ReadRetries), num(vs.ReadUnrecovered))
			line(w, "", "last mount: %s unrecovered write, %s unrecovered read errors",
				num(vs.LastMountWriteUnrecovered), num(vs.LastMountReadUnrecovered))
		}
	}

	if len(info.Problems) > 0 || len(info.Warnings()) > 0 {
		fmt.Fprintln(w)
	}
	for _, p := range info.Problems {
		fmt.Fprintf(w, "PROBLEM:  %s\n", p)
	}
	for _, warn := range info.Warnings() {
		fmt.Fprintf(w, "WARNING:  %s\n", warn)
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

func cmdDriveLog(args []string, stdout, stderr io.Writer) int {
	fs, df := newDriveFlagSet("drive log", stderr)
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
	if info.ErrorLogSlots == 0 {
		fmt.Fprintln(stdout, "The drive keeps no error log (tape diagnostic data page).")
		return exitOK
	}
	fmt.Fprintf(stdout, "Error log of %s %s (%s): %d of %d slots used, in the order the drive keeps them\n",
		info.Inquiry.Vendor, info.Inquiry.Product, info.Path, len(info.ErrorLog), info.ErrorLogSlots)
	for _, e := range info.ErrorLog {
		fmt.Fprintf(stdout, "\nSlot %d\n", e.Slot)
		problem := e.Description()
		if e.Repeated {
			problem += " (repeated)"
		}
		line(stdout, "Problem", "%s", problem)
		line(stdout, "During", "%s", e.Operation())
		cart := orDash(e.MediumID)
		if e.Format != "" {
			cart += ", " + e.Format
		}
		if c := info.Cartridge; c != nil && c.Serial != "" && c.Serial == e.MediumID {
			cart += " (the loaded cartridge)"
		}
		line(stdout, "Cartridge", "%s", cart)
		fw := orDash(e.Firmware)
		if e.Firmware != "" && e.Firmware != info.Inquiry.Revision {
			fw += fmt.Sprintf(" (now %s)", info.Inquiry.Revision)
		}
		line(stdout, "Firmware", "%s", fw)
		if w := e.When(); w != "" {
			line(stdout, "When", "%s", w)
		}
		if e.CleanHours > 0 {
			line(stdout, "Since cleaning", "%d tape hours", e.CleanHours)
		}
		line(stdout, "Codes", "sense %x/%02x/%02x, drive code 0x%08x (for the vendor's service)", e.Key, e.ASC, e.ASCQ, e.VendorCode)
	}
	var loaded string
	if info.Cartridge != nil {
		loaded = info.Cartridge.Serial
	}
	findings := drive.AnalyzeErrorLog(info.ErrorLog, loaded, info.Inquiry.Revision)
	if len(findings) > 0 {
		fmt.Fprintln(stdout, "\nAnalysis")
		for _, f := range findings {
			fmt.Fprintf(stdout, "  - %s\n", f)
		}
	}
	if len(info.ErrorLog) == 0 {
		fmt.Fprintln(stdout, "\nNo errors recorded.")
	}
	return exitOK
}

func cmdDriveSelfTest(args []string, stdout, stderr io.Writer) int {
	fs, df := newDriveFlagSet("drive selftest", stderr)
	extended := fs.Bool("extended", false, "run the extended self-test (thorough, can take much longer)")
	status := fs.Bool("status", false, "show the most recent self-test result without starting a new one")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	d, _, err := df.open()
	if err != nil {
		return fail(stderr, err)
	}
	defer d.Close()
	if err := requireTape(d); err != nil {
		return fail(stderr, err)
	}
	if *status {
		r, ok, err := drive.ReadSelfTest(d)
		if err != nil {
			return fail(stderr, err)
		}
		if !ok {
			fmt.Fprintln(stdout, "The drive reports no self-test result yet.")
			return exitOK
		}
		return reportSelfTest(stdout, r)
	}
	kind, name := drive.SelfTestShort, "short"
	if *extended {
		kind, name = drive.SelfTestExtended, "extended"
	}
	if err := drive.StartSelfTest(d, kind); err != nil {
		if errors.Is(err, drive.ErrUnsupported) {
			return fail(stderr, errors.New("the drive does not support background self-tests"))
		}
		return fail(stderr, fmt.Errorf("starting the self-test: %w", err))
	}
	fmt.Fprintf(stdout, "The drive is running its %s self-test in the background. Waiting for it to finish...\n", name)
	r, ok, err := drive.PollSelfTest(d, selfTestWait, selfTestInterval)
	if err != nil {
		fmt.Fprintf(stderr, "tapemgr: %v\nCheck later with 'tapemgr drive selftest --status'.\n", err)
		return exitFailure
	}
	if !ok {
		fmt.Fprintln(stdout, "The drive did not report a self-test result.")
		return exitOK
	}
	return reportSelfTest(stdout, r)
}

func reportSelfTest(w io.Writer, r drive.SelfTestResult) int {
	switch {
	case r.InProgress():
		fmt.Fprintln(w, "A self-test is still in progress.")
		return exitOK
	case r.Passed():
		fmt.Fprintf(w, "Self-test passed (at %d drive power-on hours).\n", r.Hours)
		return exitOK
	}
	fmt.Fprintf(w, "Self-test FAILED: %s.\n", r.Description())
	if r.Key != 0 || r.ASC != 0 || r.ASCQ != 0 {
		line(w, "Codes", "sense %x/%02x/%02x (for the vendor's service)", r.Key, r.ASC, r.ASCQ)
	}
	return exitFailure
}

func cmdDriveDensity(args []string, stdout, stderr io.Writer) int {
	fs, df := newDriveFlagSet("drive density", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	d, _, err := df.open()
	if err != nil {
		return fail(stderr, err)
	}
	defer d.Close()
	if err := requireTape(d); err != nil {
		return fail(stderr, err)
	}
	dens, err := drive.ReportDensitySupport(d)
	if err != nil {
		if errors.Is(err, drive.ErrUnsupported) {
			return fail(stderr, errors.New("the drive does not report its density support"))
		}
		return fail(stderr, fmt.Errorf("asking the drive which densities it supports: %w", err))
	}
	q, _ := drive.ReadInquiry(d)
	fmt.Fprintf(stdout, "%s %s: recording formats the drive supports\n", q.Vendor, q.Product)
	if len(dens) == 0 {
		fmt.Fprintln(stdout, "  (the drive reported none)")
	}
	for _, de := range dens {
		name := de.Generation
		if name == "" {
			name = orDash(de.Name)
		}
		rw := "read only"
		if de.Writable {
			rw = "read and write"
		}
		detail := rw
		if de.Default {
			detail += ", default"
		}
		if de.CapacityMB > 0 {
			detail += ", ~" + archive.FormatBytes(int64(de.CapacityMB)*1_000_000) + " native"
		}
		if de.Description != "" {
			detail += "  (" + de.Description + ")"
		}
		line(stdout, name, "density 0x%02x, %s", de.PrimaryCode, detail)
	}
	if enabled, ok, err := drive.CompressionEnabled(d); err == nil && ok {
		state := "off"
		if enabled {
			state = "on"
		}
		fmt.Fprintf(stdout, "\nHardware compression: %s\n", state)
	}
	return exitOK
}
