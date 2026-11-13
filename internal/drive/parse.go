package drive

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

var errShort = errors.New("response too short")

// printable keeps the ASCII text of a field, dropping padding and control
// characters a drive might return.
func printable(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c < 0x7F {
			sb.WriteByte(c)
		}
	}
	return strings.TrimSpace(sb.String())
}

func parseInquiry(b []byte) (Inquiry, error) {
	if len(b) < 36 {
		return Inquiry{}, errShort
	}
	return Inquiry{
		DeviceType: b[0] & 0x1F,
		Vendor:     printable(b[8:16]),
		Product:    printable(b[16:32]),
		Revision:   printable(b[32:36]),
	}, nil
}

func parseSerial(b []byte) (string, error) {
	if len(b) < 4 || b[1] != 0x80 {
		return "", errShort
	}
	end := min(4+int(b[3]), len(b))
	return printable(b[4:end]), nil
}

// LogParam is one parameter of a log page.
type LogParam struct {
	Code  uint16
	Value []byte
}

// Uint returns the value as an unsigned big-endian number. ok is false if
// it is empty or longer than 8 bytes.
func (p LogParam) Uint() (v uint64, ok bool) {
	if len(p.Value) == 0 || len(p.Value) > 8 {
		return 0, false
	}
	for _, c := range p.Value {
		v = v<<8 | uint64(c)
	}
	return v, true
}

// LogPage is the parameters of a log page in the order the drive sent them.
type LogPage []LogParam

// Get returns the parameter with the given code.
func (lp LogPage) Get(code uint16) (LogParam, bool) {
	for _, p := range lp {
		if p.Code == code {
			return p, true
		}
	}
	return LogParam{}, false
}

// Uint returns a numeric parameter.
func (lp LogPage) Uint(code uint16) (uint64, bool) {
	p, ok := lp.Get(code)
	if !ok {
		return 0, false
	}
	return p.Uint()
}

func parseLogPage(b []byte, page byte) (LogPage, error) {
	if len(b) < 4 {
		return nil, errShort
	}
	if b[0]&0x3F != page {
		return nil, fmt.Errorf("drive returned log page 0x%02x instead of 0x%02x", b[0]&0x3F, page)
	}
	// The page length may claim more than was transferred; only what was
	// received is parsed.
	end := min(4+int(binary.BigEndian.Uint16(b[2:])), len(b))
	var lp LogPage
	for off := 4; off+4 <= end; {
		code := binary.BigEndian.Uint16(b[off:])
		n := int(b[off+3])
		if off+4+n > end {
			break // torn last parameter
		}
		lp = append(lp, LogParam{Code: code, Value: b[off+4 : off+4+n]})
		off += 4 + n
	}
	return lp, nil
}

// Attribute is one cartridge memory (MAM) attribute.
type Attribute struct {
	ID       uint16
	ReadOnly bool
	Format   byte // 0 binary, 1 ASCII, 2 text
	Value    []byte
}

// Attributes are the MAM attributes of one partition.
type Attributes map[uint16]Attribute

// MAM attribute identifiers (SPC-4).
const (
	AttrRemainingCapacity = 0x0000 // MiB, per partition
	AttrMaximumCapacity   = 0x0001 // MiB, per partition
	AttrLoadCount         = 0x0003
	AttrTotalWritten      = 0x0220 // MiB over the cartridge's life
	AttrTotalRead         = 0x0221 // MiB over the cartridge's life
	AttrManufacturer      = 0x0400
	AttrSerial            = 0x0401
	AttrManufactureDate   = 0x0406 // YYYYMMDD
	AttrMediumType        = 0x0408
	AttrDensity           = 0x0405
	AttrAppVendor         = 0x0800
	AttrAppName           = 0x0801
	AttrAppVersion        = 0x0802
	AttrUserLabel         = 0x0803
	AttrBarcode           = 0x0806
	AttrVolumeCoherency   = 0x080C
)

// String returns a text attribute.
func (a Attributes) String(id uint16) string {
	v, ok := a[id]
	if !ok {
		return ""
	}
	return printable(v.Value)
}

// Uint returns a binary attribute of at most 8 bytes.
func (a Attributes) Uint(id uint16) (uint64, bool) {
	v, ok := a[id]
	if !ok {
		return 0, false
	}
	return LogParam{Value: v.Value}.Uint()
}

func parseAttributes(b []byte) (Attributes, error) {
	if len(b) < 4 {
		return nil, errShort
	}
	end := int(min(uint64(binary.BigEndian.Uint32(b))+4, uint64(len(b))))
	attrs := Attributes{}
	for off := 4; off+5 <= end; {
		id := binary.BigEndian.Uint16(b[off:])
		n := int(binary.BigEndian.Uint16(b[off+3:]))
		if off+5+n > end {
			break
		}
		attrs[id] = Attribute{
			ID:       id,
			ReadOnly: b[off+2]&0x80 != 0,
			Format:   b[off+2] & 0x03,
			Value:    b[off+5 : off+5+n],
		}
		off += 5 + n
	}
	return attrs, nil
}

// Log pages used here.
const (
	PageWriteErrors  = 0x02
	PageReadErrors   = 0x03
	PageDeviceStatus = 0x11 // DT device status, holds the VHF data
	PageVolumeStats  = 0x17
	PageProtocolPort = 0x18 // protocol specific port, holds SAS phy link rates
	PageTapeAlert    = 0x2E
)

// ErrorCounters are the error counter log pages (0x02 write, 0x03 read).
type ErrorCounters struct {
	Corrected     uint64 // errors corrected by the drive, total
	Uncorrected   uint64 // errors the drive could not correct
	Bytes         uint64 // bytes processed
	HaveCorrected bool
	HaveUncorr    bool
	HaveBytes     bool
}

func parseErrorCounters(lp LogPage) ErrorCounters {
	var c ErrorCounters
	c.Corrected, c.HaveCorrected = lp.Uint(0x0003)
	c.Bytes, c.HaveBytes = lp.Uint(0x0005)
	c.Uncorrected, c.HaveUncorr = lp.Uint(0x0006)
	return c
}

// VHF is the very high frequency data of the DT device status page (SSC-4):
// a summary of the drive's state updated continuously.
type VHF struct {
	CleanRequested bool // CRQST: the drive asks for a cleaning cartridge
	CleanRequired  bool // CRQRD: the drive needs cleaning to keep working
	WriteProtect   bool // WRTP
	Compression    bool // CMPR
	MediumPresent  bool // MPRSNT
	Threaded       bool // MTHRD: tape is threaded through the drive
	Mounted        bool // MOUNTED
	InTransition   bool // INXTN: a load or unload is in progress
	Activity       byte // DT device activity code
}

func parseVHF(lp LogPage) (VHF, error) {
	p, ok := lp.Get(0x0000)
	if !ok || len(p.Value) < 4 {
		return VHF{}, errors.New("no VHF data")
	}
	b := p.Value
	return VHF{
		CleanRequested: b[0]&0x04 != 0,
		CleanRequired:  b[0]&0x02 != 0,
		WriteProtect:   b[0]&0x08 != 0,
		Compression:    b[0]&0x10 != 0,
		InTransition:   b[1]&0x80 != 0,
		MediumPresent:  b[1]&0x10 != 0,
		Threaded:       b[1]&0x02 != 0,
		Mounted:        b[1]&0x01 != 0,
		Activity:       b[2],
	}, nil
}

var activityNames = map[byte]string{
	0x00: "idle",
	0x01: "cleaning",
	0x02: "loading",
	0x03: "unloading",
	0x04: "other medium activity",
	0x05: "reading",
	0x06: "writing",
	0x07: "locating",
	0x08: "rewinding",
	0x09: "erasing",
	0x0A: "formatting",
	0x0B: "calibrating",
	0x0C: "other activity",
	0x0D: "updating microcode",
	0x0E: "reading encrypted data",
	0x0F: "writing encrypted data",
}

// ActivityName describes what the drive is doing.
func (v VHF) ActivityName() string {
	if s, ok := activityNames[v.Activity]; ok {
		return s
	}
	return fmt.Sprintf("activity 0x%02x", v.Activity)
}

// CleaningWarning returns a message when the drive is asking to be cleaned,
// or "" when it is not. The clean-required state is reported ahead of a
// clean-requested one, as it is the more urgent of the two.
func (v VHF) CleaningWarning() string {
	switch {
	case v.CleanRequired:
		return "drive needs cleaning now (VHF clean required); load a cleaning cartridge"
	case v.CleanRequested:
		return "drive requests cleaning (VHF clean requested); load a cleaning cartridge soon"
	}
	return ""
}

// LinkRate is the negotiated link rate of one SAS phy, read from the protocol
// specific port log page. It identifies the phy by its relative target port
// and phy number, so a drive with several phys or ports is reported fully.
type LinkRate struct {
	Port uint16 // relative target port, the log parameter code
	Phy  byte
	Rate string // for example "6 Gbps", or a phy state such as "disabled"
}

// Where returns a stable label for one phy, used to spot a rate change.
func (l LinkRate) Where() string {
	return fmt.Sprintf("port %d phy %d", l.Port, l.Phy)
}

// linkRateNames decodes the negotiated logical link rate nibble (SPL). The
// speed values are what change under a cable or backplane fault; the rest are
// phy states reported in the same field.
var linkRateNames = map[byte]string{
	0x0: "unknown rate",
	0x1: "disabled",
	0x2: "reset problem",
	0x3: "spinup hold",
	0x4: "port selector",
	0x5: "reset in progress",
	0x6: "unsupported phy attached",
	0x8: "1.5 Gbps",
	0x9: "3 Gbps",
	0xA: "6 Gbps",
	0xB: "12 Gbps",
	0xC: "22.5 Gbps",
}

func linkRateName(code byte) string {
	if s, ok := linkRateNames[code]; ok {
		return s
	}
	return fmt.Sprintf("rate 0x%x", code)
}

// parseProtocolPort reads the negotiated link rate of each SAS phy from the
// protocol specific port log page. Non-SAS ports (a Fibre Channel attachment
// uses the same page differently) are skipped. A descriptor whose length does
// not fit stops that port, so a short or damaged page yields what was read.
func parseProtocolPort(lp LogPage) []LinkRate {
	var out []LinkRate
	for _, p := range lp {
		v := p.Value
		// SAS protocol specific port parameter: protocol identifier (SAS is
		// 6), reserved, generation code, number of phys, then phy descriptors.
		if len(v) < 4 || v[0]&0x0F != 6 {
			continue
		}
		nphys := int(v[3])
		off := 4
		for i := 0; i < nphys; i++ {
			if off+6 > len(v) {
				break
			}
			descLen := int(v[off+3]) + 4
			if descLen < 6 || off+descLen > len(v) {
				break
			}
			out = append(out, LinkRate{
				Port: p.Code,
				Phy:  v[off+1],
				// Byte 5 of the descriptor: high nibble reason, low nibble
				// negotiated logical link rate.
				Rate: linkRateName(v[off+5] & 0x0F),
			})
			off += descLen
		}
	}
	return out
}

// Alert is an active TapeAlert flag.
type Alert struct {
	Flag     int
	Name     string
	Severity Severity
}

// Severity of a TapeAlert flag.
type Severity byte

const (
	SevInfo     Severity = 'I'
	SevWarning  Severity = 'W'
	SevCritical Severity = 'C'
)

func (s Severity) String() string {
	switch s {
	case SevCritical:
		return "critical"
	case SevWarning:
		return "warning"
	}
	return "info"
}

// tapeAlerts names the TapeAlert flags of tape drives (SSC-4 annex).
var tapeAlerts = map[int]struct {
	name string
	sev  Severity
}{
	1:  {"read warning: drive has trouble reading", SevWarning},
	2:  {"write warning: drive has trouble writing", SevWarning},
	3:  {"hard error: read or write failed", SevWarning},
	4:  {"media: data on the cartridge is at risk", SevCritical},
	5:  {"read failure", SevCritical},
	6:  {"write failure", SevCritical},
	7:  {"media life: cartridge has reached its end of life", SevWarning},
	8:  {"cartridge is not data grade", SevWarning},
	9:  {"cartridge is write protected", SevCritical},
	10: {"cartridge removal prevented", SevInfo},
	11: {"cleaning cartridge loaded", SevInfo},
	12: {"unsupported cartridge format", SevInfo},
	13: {"recoverable mechanical cartridge failure", SevCritical},
	14: {"unrecoverable mechanical cartridge failure", SevCritical},
	15: {"cartridge memory chip failure", SevWarning},
	16: {"cartridge was forcibly ejected", SevCritical},
	17: {"read-only cartridge format", SevWarning},
	18: {"tape directory corrupted on load", SevWarning},
	19: {"cartridge is nearing end of life", SevInfo},
	20: {"clean now: drive needs cleaning", SevCritical},
	21: {"clean periodic: drive is due for routine cleaning", SevWarning},
	22: {"cleaning cartridge is expired", SevCritical},
	23: {"invalid cleaning cartridge", SevCritical},
	24: {"retension requested", SevWarning},
	25: {"dual-port interface error", SevWarning},
	26: {"cooling fan failure", SevWarning},
	27: {"power supply failure", SevWarning},
	28: {"power consumption out of range", SevWarning},
	29: {"drive needs preventive maintenance", SevWarning},
	30: {"drive hardware failure (A)", SevCritical},
	31: {"drive hardware failure (B)", SevCritical},
	32: {"host interface problem", SevWarning},
	33: {"eject cartridge and retry", SevCritical},
	34: {"firmware download failed", SevWarning},
	35: {"drive humidity out of range", SevWarning},
	36: {"drive temperature out of range", SevWarning},
	37: {"drive voltage out of range", SevWarning},
	38: {"predictive failure of drive hardware", SevCritical},
	39: {"drive needs diagnostics", SevWarning},
	49: {"lost statistics", SevWarning},
	50: {"tape directory invalid at unload", SevWarning},
	51: {"tape system area write failure", SevCritical},
	52: {"tape system area read failure", SevCritical},
	53: {"no start of data", SevCritical},
	54: {"loading failure", SevCritical},
	55: {"unrecoverable unload failure", SevCritical},
	56: {"automation interface failure", SevCritical},
	57: {"firmware failure", SevWarning},
	58: {"WORM integrity check failed", SevWarning},
	59: {"WORM overwrite attempted", SevWarning},
}

func parseTapeAlerts(lp LogPage) []Alert {
	var out []Alert
	for _, p := range lp {
		if p.Code < 1 || p.Code > 64 || len(p.Value) < 1 || p.Value[0]&1 == 0 {
			continue
		}
		a := Alert{Flag: int(p.Code), Name: fmt.Sprintf("TapeAlert flag %d", p.Code), Severity: SevWarning}
		if t, ok := tapeAlerts[a.Flag]; ok {
			a.Name, a.Severity = t.name, t.sev
		}
		out = append(out, a)
	}
	return out
}
