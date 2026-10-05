package drive

import (
	"errors"
	"fmt"
	"regexp"
)

// Info is everything known about a drive and its cartridge.
type Info struct {
	Path      string
	Inquiry   Inquiry
	Serial    string
	Firmware  FirmwareBuild // zero if the drive does not describe it
	Ready     error         // nil when a cartridge is loaded and ready
	Cartridge *Cartridge    // nil without a cartridge
	VHF       *VHF          // nil if the drive does not report it
	Alerts    []Alert
	// Error counters of the drive since it was powered on or the cartridge
	// was loaded, depending on the drive. nil if not reported.
	WriteErrors, ReadErrors *ErrorCounters
	Stats                   *DriveStats       // lifetime statistics, nil if not reported
	Compression             *Compression      // since the cartridge was loaded, nil if not reported
	NonMediumErrors         Count             // errors not caused by the tape, such as interface errors
	CleaningRequired        bool              // sequential access page: cleaning action required
	ErrorLog                []LogEntry        // the drive\'s error history (used slots)
	ErrorLogSlots           int               // 0 if the drive keeps no such log
	Encryption              *EncryptionStatus // nil if the drive has no encryption
	Algorithms              []Algorithm
	Problems                []string // information that could not be read
}

// Cartridge describes the loaded cartridge from its memory chip (MAM) and
// the volume statistics log page.
type Cartridge struct {
	Serial          string
	Barcode         string
	Manufacturer    string
	ManufactureDate string // YYYYMMDD
	Format          string // LTO generation from the density code
	Kind            string // data, cleaning or WORM
	LoadCount       uint64
	Partitions      []Capacity
	WrittenMiB      uint64 // over the cartridge's life
	ReadMiB         uint64
	Application     string       // software that last formatted it, e.g. "IBM LTFS 2.4.9.0"
	LTFSVolume      string       // LTFS volume UUID from the volume coherency info
	Stats           *VolumeStats // nil if the drive does not report them
	WriteProtected  bool
}

// Capacity of one partition in MiB.
type Capacity struct {
	RemainingMiB, MaximumMiB uint64
}

var densityNames = map[byte]string{
	0x40: "LTO-1", 0x42: "LTO-2", 0x44: "LTO-3", 0x46: "LTO-4",
	0x58: "LTO-5", 0x5A: "LTO-6", 0x5C: "LTO-7", 0x5D: "LTO-7 Type M (M8)",
	0x5E: "LTO-8", 0x60: "LTO-9",
}

// Gather reads drive and cartridge information. Parts that cannot be read
// are listed in Problems; only an unusable device is an error.
func Gather(d Device) (*Info, error) {
	info := &Info{Path: d.Path()}
	q, err := ReadInquiry(d)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.Path(), err)
	}
	info.Inquiry = q
	if !q.IsTape() {
		return nil, fmt.Errorf("%s is not a tape drive (device type 0x%02x)", d.Path(), q.DeviceType)
	}
	problem := func(what string, err error) {
		info.Problems = append(info.Problems, fmt.Sprintf("%s: %v", what, err))
	}
	if info.Serial, err = ReadSerial(d); err != nil {
		problem("serial number", err)
	}
	if fb, err := ReadFirmwareBuild(d, q); err == nil {
		info.Firmware = fb
	}
	info.Ready = TestUnitReady(d)

	if lp, err := ReadLogPage(d, PageDeviceStatus); err == nil {
		if v, err := parseVHF(lp); err == nil {
			info.VHF = &v
		} else {
			problem("device status", err)
		}
	} else if !errors.Is(err, ErrUnsupported) {
		problem("device status", err)
	}
	if lp, err := ReadLogPage(d, PageTapeAlert); err == nil {
		info.Alerts = parseTapeAlerts(lp)
	} else if !errors.Is(err, ErrUnsupported) {
		problem("TapeAlert", err)
	}
	if lp, err := ReadLogPage(d, PageDeviceStats); err == nil {
		info.Stats = parseDriveStats(lp)
	} else if !errors.Is(err, ErrUnsupported) {
		problem("device statistics", err)
	}
	if lp, err := ReadLogPage(d, PageCompression); err == nil {
		info.Compression = parseCompression(lp)
	} else if !errors.Is(err, ErrUnsupported) {
		problem("compression statistics", err)
	}
	if lp, err := ReadLogPage(d, PageNonMediumErrors); err == nil {
		info.NonMediumErrors = count(lp, 0x0000)
	} else if !errors.Is(err, ErrUnsupported) {
		problem("non-medium errors", err)
	}
	if lp, err := ReadLogPage(d, PageSequential); err == nil {
		if v, ok := lp.Uint(0x0100); ok && v != 0 {
			info.CleaningRequired = true
		}
	} else if !errors.Is(err, ErrUnsupported) {
		problem("sequential access statistics", err)
	}
	if entries, slots, err := ReadErrorLog(d); err == nil {
		info.ErrorLog, info.ErrorLogSlots = entries, slots
	} else if !errors.Is(err, ErrUnsupported) {
		problem("error log", err)
	}
	for _, c := range []struct {
		page byte
		dst  **ErrorCounters
		name string
	}{{PageWriteErrors, &info.WriteErrors, "write error counters"}, {PageReadErrors, &info.ReadErrors, "read error counters"}} {
		lp, err := ReadLogPage(d, c.page)
		if err != nil {
			if !errors.Is(err, ErrUnsupported) {
				problem(c.name, err)
			}
			continue
		}
		ec := parseErrorCounters(lp)
		*c.dst = &ec
	}

	if algs, err := ReadEncryptionAlgorithms(d); err == nil {
		info.Algorithms = algs
		if st, err := ReadEncryptionStatus(d); err == nil {
			info.Encryption = st
		} else if !errors.Is(err, ErrUnsupported) {
			problem("encryption status", err)
		}
	} else if !errors.Is(err, ErrUnsupported) {
		problem("encryption capabilities", err)
	}

	if info.Ready == nil || !errors.Is(info.Ready, ErrNoMedium) {
		if c, err := readCartridge(d); err == nil {
			info.Cartridge = c
		} else if !errors.Is(err, ErrNoMedium) {
			problem("cartridge memory", err)
		}
	}
	return info, nil
}

func readCartridge(d Device) (*Cartridge, error) {
	a, err := ReadAttributes(d, 0)
	if err != nil {
		return nil, err
	}
	c := &Cartridge{
		Serial:          a.String(AttrSerial),
		Barcode:         a.String(AttrBarcode),
		Manufacturer:    a.String(AttrManufacturer),
		ManufactureDate: a.String(AttrManufactureDate),
	}
	c.LoadCount, _ = a.Uint(AttrLoadCount)
	c.WrittenMiB, _ = a.Uint(AttrTotalWritten)
	c.ReadMiB, _ = a.Uint(AttrTotalRead)
	if v, ok := a[AttrDensity]; ok && len(v.Value) == 1 {
		c.Format = densityNames[v.Value[0]]
		if c.Format == "" {
			c.Format = fmt.Sprintf("density 0x%02x", v.Value[0])
		}
	}
	if v, ok := a[AttrMediumType]; ok && len(v.Value) == 1 {
		switch v.Value[0] {
		case 0x00:
			c.Kind = "data"
		case 0x01:
			c.Kind = "cleaning"
		case 0x80:
			c.Kind = "WORM"
		default:
			c.Kind = fmt.Sprintf("type 0x%02x", v.Value[0])
		}
	}
	if name := a.String(AttrAppName); name != "" {
		c.Application = joinNonEmpty(a.String(AttrAppVendor), name, a.String(AttrAppVersion))
	}
	if v, ok := a[AttrVolumeCoherency]; ok {
		c.LTFSVolume = parseCoherencyUUID(v.Value)
	}
	if wp, err := WriteProtected(d); err == nil {
		c.WriteProtected = wp
	}
	c.Partitions = append(c.Partitions, capacityOf(a))
	// LTFS uses two partitions: a small index partition and the data
	// partition. A single-partition cartridge rejects partition 1.
	if a1, err := ReadAttributes(d, 1); err == nil {
		if _, ok := a1[AttrMaximumCapacity]; ok {
			c.Partitions = append(c.Partitions, capacityOf(a1))
		}
	}
	if lp, err := ReadLogPage(d, PageVolumeStats); err == nil {
		c.Stats = parseVolumeStats(lp)
	}
	return c, nil
}

func capacityOf(a Attributes) Capacity {
	var c Capacity
	c.RemainingMiB, _ = a.Uint(AttrRemainingCapacity)
	c.MaximumMiB, _ = a.Uint(AttrMaximumCapacity)
	return c
}

func joinNonEmpty(parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += " "
		}
		out += p
	}
	return out
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// parseCoherencyUUID extracts the LTFS volume UUID from the volume coherency
// information attribute LTFS writes: a volume change reference, coherency
// count and set identifier, then application data "LTFS", one byte, and the
// 36 character UUID.
func parseCoherencyUUID(b []byte) string {
	if len(b) < 1 {
		return ""
	}
	off := 1 + int(b[0]) + 8 + 8 // VCR length, VCR, count, set identifier
	if off+2 > len(b) {
		return ""
	}
	n := int(b[off])<<8 | int(b[off+1])
	off += 2
	if off+n > len(b) || n < 5+36 {
		return ""
	}
	acsi := b[off : off+n]
	if string(acsi[:4]) != "LTFS" {
		return ""
	}
	if u := string(acsi[5 : 5+36]); uuidPattern.MatchString(u) {
		return u
	}
	return ""
}

// Warnings lists conditions that need attention: cleaning requests,
// TapeAlert flags of warning or critical severity, and errors the drive
// could not correct.
func (in *Info) Warnings() []string {
	var w []string
	if in.VHF != nil {
		if in.VHF.CleanRequired {
			w = append(w, "drive needs cleaning now (VHF clean required); load a cleaning cartridge")
		} else if in.VHF.CleanRequested {
			w = append(w, "drive requests cleaning (VHF clean requested); load a cleaning cartridge soon")
		}
	}
	for _, a := range in.Alerts {
		if a.Severity != SevInfo {
			w = append(w, fmt.Sprintf("TapeAlert %d (%s): %s", a.Flag, a.Severity, a.Name))
		}
	}
	if in.WriteErrors != nil && in.WriteErrors.Uncorrected > 0 {
		w = append(w, fmt.Sprintf("drive reports %d uncorrected write errors", in.WriteErrors.Uncorrected))
	}
	if in.ReadErrors != nil && in.ReadErrors.Uncorrected > 0 {
		w = append(w, fmt.Sprintf("drive reports %d uncorrected read errors", in.ReadErrors.Uncorrected))
	}
	if in.CleaningRequired && (in.VHF == nil || !in.VHF.CleanRequired) {
		w = append(w, "drive reports that cleaning is required; load a cleaning cartridge")
	}
	if in.Stats != nil && in.Stats.TemperatureExceeded {
		w = append(w, "drive mechanism has exceeded its maximum recommended temperature; check cooling")
	}
	if c := in.Cartridge; c != nil && c.Serial != "" {
		n := 0
		for _, e := range in.ErrorLog {
			if e.MediumID == c.Serial && e.Key == SenseMediumError {
				n++
			}
		}
		if n > 0 {
			w = append(w, fmt.Sprintf("the drive's error log has %d medium error(s) for the loaded cartridge; verify it and copy what you need", n))
		}
	}
	if c := in.Cartridge; c != nil && c.Stats != nil {
		e := c.Stats
		if e.WriteUnrecovered.N > 0 || e.ReadUnrecovered.N > 0 {
			w = append(w, fmt.Sprintf("tape has had %d unrecovered write and %d unrecovered read errors over its life; verify it and consider copying it", e.WriteUnrecovered.N, e.ReadUnrecovered.N))
		}
		if e.TemperatureExceeded {
			w = append(w, "tape has exceeded its maximum recommended temperature")
		}
	}
	return w
}

// EncryptionAlgorithm names the algorithm the drive currently encrypts
// with, or "" if unknown.
func (in *Info) EncryptionAlgorithm() string {
	if in.Encryption == nil {
		return ""
	}
	for _, a := range in.Algorithms {
		if a.Index == in.Encryption.AlgorithmIndex {
			return a.Name()
		}
	}
	return ""
}
