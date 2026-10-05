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
	Ready     error      // nil when a cartridge is loaded and ready
	Cartridge *Cartridge // nil without a cartridge
	VHF       *VHF       // nil if the drive does not report it
	Alerts    []Alert
	// Error counters of the drive since it was powered on or the cartridge
	// was loaded, depending on the drive. nil if not reported.
	WriteErrors, ReadErrors *ErrorCounters
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
	Application     string // software that last formatted it, e.g. "IBM LTFS 2.4.9.0"
	LTFSVolume      string // LTFS volume UUID from the volume coherency info
	Errors          *VolumeErrors
}

// Capacity of one partition in MiB.
type Capacity struct {
	RemainingMiB, MaximumMiB uint64
}

// VolumeErrors are the error counts the drive keeps for the cartridge in
// the volume statistics log page (SSC-4).
type VolumeErrors struct {
	WriteRetries, WriteUnrecovered uint64 // over the cartridge's life
	ReadRetries, ReadUnrecovered   uint64
	// Unrecovered errors during the last mount of the cartridge.
	LastMountWriteUnrecovered, LastMountReadUnrecovered uint64
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
	c.Partitions = append(c.Partitions, capacityOf(a))
	// LTFS uses two partitions: a small index partition and the data
	// partition. A single-partition cartridge rejects partition 1.
	if a1, err := ReadAttributes(d, 1); err == nil {
		if _, ok := a1[AttrMaximumCapacity]; ok {
			c.Partitions = append(c.Partitions, capacityOf(a1))
		}
	}
	if lp, err := ReadLogPage(d, PageVolumeStats); err == nil {
		c.Errors = parseVolumeErrors(lp)
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

func parseVolumeErrors(lp LogPage) *VolumeErrors {
	get := func(code uint16) uint64 { v, _ := lp.Uint(code); return v }
	if _, ok := lp.Get(0x0003); !ok {
		return nil
	}
	return &VolumeErrors{
		WriteRetries:              get(0x0003),
		WriteUnrecovered:          get(0x0004),
		ReadRetries:               get(0x0008),
		ReadUnrecovered:           get(0x0009),
		LastMountWriteUnrecovered: get(0x000C),
		LastMountReadUnrecovered:  get(0x000D),
	}
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
	if c := in.Cartridge; c != nil && c.Errors != nil {
		e := c.Errors
		if e.WriteUnrecovered > 0 || e.ReadUnrecovered > 0 {
			w = append(w, fmt.Sprintf("cartridge has had %d unrecovered write and %d unrecovered read errors over its life; verify it and consider copying it", e.WriteUnrecovered, e.ReadUnrecovered))
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
