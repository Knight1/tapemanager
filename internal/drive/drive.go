// Package drive talks to LTO tape drives directly with SCSI commands
// through the Linux SCSI generic (sg) interface, without external tools.
// It reads drive and cartridge information, error counters, TapeAlert flags
// and cleaning requests, and loads or ejects cartridges.
//
// Everything a drive returns is treated as untrusted: every length field is
// checked against the bytes actually received.
package drive

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Direction is the data direction of a command.
type Direction int

const (
	DirNone Direction = iota
	DirIn             // from the drive
	DirOut            // to the drive
)

// Device sends SCSI commands to a drive.
type Device interface {
	Do(cdb []byte, dir Direction, buf []byte, timeout time.Duration) (int, error)
	Path() string
	Close() error
}

// Sense keys used to classify errors.
const (
	SenseNoSense        = 0x0
	SenseRecovered      = 0x1
	SenseNotReady       = 0x2
	SenseMediumError    = 0x3
	SenseHardwareError  = 0x4
	SenseIllegalRequest = 0x5
	SenseUnitAttention  = 0x6
	SenseDataProtect    = 0x7
)

var senseKeyNames = [16]string{
	"no sense", "recovered error", "not ready", "medium error",
	"hardware error", "illegal request", "unit attention", "data protect",
	"blank check", "vendor specific", "copy aborted", "aborted command",
	"reserved", "volume overflow", "miscompare", "completed",
}

// CommandError is a command that the drive rejected or failed.
type CommandError struct {
	Op        byte
	Status    byte // SCSI status, 0x02 is CHECK CONDITION
	Key       byte
	ASC, ASCQ byte
}

func (e *CommandError) Error() string {
	if e.Status != 0x02 && e.Key == 0 && e.ASC == 0 {
		return fmt.Sprintf("command 0x%02x failed: SCSI status 0x%02x", e.Op, e.Status)
	}
	msg := fmt.Sprintf("command 0x%02x: %s", e.Op, senseKeyNames[e.Key&0xF])
	if d := ascText(e.ASC, e.ASCQ); d != "" {
		return msg + ": " + d
	}
	return msg + fmt.Sprintf(" (ASC 0x%02x, ASCQ 0x%02x)", e.ASC, e.ASCQ)
}

// Is matches the sentinel errors below.
func (e *CommandError) Is(target error) bool {
	switch target {
	case ErrNoMedium:
		return e.Key == SenseNotReady && e.ASC == 0x3A
	case ErrUnsupported:
		return e.Key == SenseIllegalRequest && (e.ASC == 0x20 || e.ASC == 0x24)
	case ErrRemovalPrevented:
		return e.Key == SenseIllegalRequest && e.ASC == 0x53 && e.ASCQ == 0x02
	}
	return false
}

var (
	ErrNoMedium         = errors.New("no cartridge in the drive")
	ErrUnsupported      = errors.New("not supported by the drive")
	ErrRemovalPrevented = errors.New("cartridge removal is prevented (is the tape still mounted?)")
)

func newCommandError(op, status byte, sense []byte) *CommandError {
	e := &CommandError{Op: op, Status: status}
	if len(sense) < 1 {
		return e
	}
	switch sense[0] & 0x7F {
	case 0x70, 0x71: // fixed format
		if len(sense) > 2 {
			e.Key = sense[2] & 0x0F
		}
		if len(sense) > 13 {
			e.ASC, e.ASCQ = sense[12], sense[13]
		}
	case 0x72, 0x73: // descriptor format
		if len(sense) > 3 {
			e.Key, e.ASC, e.ASCQ = sense[1]&0x0F, sense[2], sense[3]
		}
	}
	return e
}

// ascText describes the additional sense codes a tape user is likely to see.
func ascText(asc, ascq byte) string {
	switch uint16(asc)<<8 | uint16(ascq) {
	case 0x0401:
		return "becoming ready"
	case 0x0402:
		return "load needed"
	case 0x0412:
		return "drive offline"
	case 0x0300:
		return "write fault"
	case 0x0900:
		return "track following error"
	case 0x0C00:
		return "write error"
	case 0x1100:
		return "unrecovered read error"
	case 0x1400:
		return "recorded data not found"
	case 0x1500:
		return "positioning error"
	case 0x2000:
		return "invalid command"
	case 0x2400:
		return "invalid field in command"
	case 0x2600:
		return "invalid field in parameter list"
	case 0x2700:
		return "write protected"
	case 0x2800:
		return "cartridge changed"
	case 0x2900:
		return "reset occurred"
	case 0x3000:
		return "incompatible cartridge"
	case 0x3003:
		return "cleaning cartridge installed"
	case 0x3100:
		return "medium format corrupted"
	case 0x3A00:
		return "no cartridge"
	case 0x3B00:
		return "sequential positioning error"
	case 0x5100:
		return "erase failure"
	case 0x5200:
		return "cartridge fault"
	case 0x5300:
		return "cartridge load or eject failed"
	case 0x3B0E:
		return "medium source element empty"
	case 0x4400:
		return "internal target failure"
	case 0x5302:
		return "medium removal prevented"
	case 0x5500:
		return "system resource failure"
	}
	return ""
}

const (
	shortTimeout = 60 * time.Second
	moveTimeout  = 15 * time.Minute // load, unload and rewind
)

// Command operation codes.
const (
	opTestUnitReady = 0x00
	opInquiry       = 0x12
	opModeSense6    = 0x1A
	opLoadUnload    = 0x1B
	opLogSense      = 0x4D
	opReadAttribute = 0x8C
)

func inquiryCDB(evpd bool, page byte, alloc uint16) []byte {
	cdb := make([]byte, 6)
	cdb[0] = opInquiry
	if evpd {
		cdb[1] = 1
		cdb[2] = page
	}
	binary.BigEndian.PutUint16(cdb[3:], alloc)
	return cdb
}

func logSenseCDB(page byte, alloc uint16) []byte {
	cdb := make([]byte, 10)
	cdb[0] = opLogSense
	cdb[2] = 0x40 | page&0x3F // PC=01b: current cumulative values
	binary.BigEndian.PutUint16(cdb[7:], alloc)
	return cdb
}

func readAttributeCDB(partition byte, first uint16, alloc uint32) []byte {
	cdb := make([]byte, 16)
	cdb[0] = opReadAttribute
	cdb[1] = 0x00 // service action: attribute values
	cdb[7] = partition
	binary.BigEndian.PutUint16(cdb[8:], first)
	binary.BigEndian.PutUint32(cdb[10:], alloc)
	return cdb
}

func loadUnloadCDB(load bool) []byte {
	cdb := make([]byte, 6)
	cdb[0] = opLoadUnload
	if load {
		cdb[4] = 1
	}
	return cdb
}

// TestUnitReady returns nil if a cartridge is loaded and ready.
func TestUnitReady(d Device) error {
	var err error
	// A pending unit attention (cartridge changed, reset) is reported once;
	// asking again gives the real state.
	for range 3 {
		_, err = d.Do([]byte{opTestUnitReady, 0, 0, 0, 0, 0}, DirNone, nil, shortTimeout)
		var ce *CommandError
		if !errors.As(err, &ce) || ce.Key != SenseUnitAttention {
			return err
		}
	}
	return err
}

// Inquiry is the drive's identity.
type Inquiry struct {
	DeviceType byte // 0x01 is a tape drive
	Vendor     string
	Product    string
	Revision   string // firmware level
}

// IsTape reports whether the device is a sequential access (tape) device.
func (q Inquiry) IsTape() bool { return q.DeviceType == 0x01 }

// ReadInquiry reads the standard INQUIRY data.
func ReadInquiry(d Device) (Inquiry, error) {
	buf := make([]byte, 96)
	n, err := d.Do(inquiryCDB(false, 0, uint16(len(buf))), DirIn, buf, shortTimeout)
	if err != nil {
		return Inquiry{}, err
	}
	return parseInquiry(buf[:n])
}

// ReadSerial reads the drive's serial number (VPD page 0x80).
func ReadSerial(d Device) (string, error) {
	buf := make([]byte, 255)
	n, err := d.Do(inquiryCDB(true, 0x80, uint16(len(buf))), DirIn, buf, shortTimeout)
	if err != nil {
		return "", err
	}
	return parseSerial(buf[:n])
}

// ReadLogPage reads one log page and returns its parameters.
func ReadLogPage(d Device, page byte) (LogPage, error) {
	buf := make([]byte, 0xFFFC)
	n, err := d.Do(logSenseCDB(page, uint16(len(buf))), DirIn, buf, shortTimeout)
	if err != nil {
		return nil, err
	}
	return parseLogPage(buf[:n], page)
}

// ReadAttributes reads the cartridge memory (MAM) attributes of a partition.
func ReadAttributes(d Device, partition byte) (Attributes, error) {
	buf := make([]byte, 0x10000)
	n, err := d.Do(readAttributeCDB(partition, 0, uint32(len(buf))), DirIn, buf, shortTimeout)
	if err != nil {
		return nil, err
	}
	return parseAttributes(buf[:n])
}

// WriteProtected reports whether the loaded cartridge is write protected,
// from the WP bit of the MODE SENSE header, or the VHF data if the drive
// does not answer MODE SENSE. WORM cartridges that cannot be appended to
// report it the same way.
func WriteProtected(d Device) (bool, error) {
	buf := make([]byte, 255)
	// All pages without block descriptors; only the header is used.
	n, err := d.Do([]byte{opModeSense6, 0x08, 0x3F, 0, byte(len(buf)), 0}, DirIn, buf, shortTimeout)
	if err == nil {
		if n < 4 {
			return false, errShort
		}
		return buf[2]&0x80 != 0, nil
	}
	if errors.Is(err, ErrNoMedium) {
		return false, err
	}
	lp, lerr := ReadLogPage(d, PageDeviceStatus)
	if lerr != nil {
		return false, err
	}
	v, lerr := parseVHF(lp)
	if lerr != nil {
		return false, err
	}
	return v.WriteProtect, nil
}

// Load loads the cartridge in the drive.
func Load(d Device) error {
	_, err := d.Do(loadUnloadCDB(true), DirNone, nil, moveTimeout)
	return err
}

// Unload rewinds and ejects the cartridge.
func Unload(d Device) error {
	_, err := d.Do(loadUnloadCDB(false), DirNone, nil, moveTimeout)
	return err
}
