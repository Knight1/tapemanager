package drive

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// The power condition mode page (0x1A) and MODE SELECT. There is no host
// command that puts a tape drive to sleep at once (opcode 0x1B is LOAD
// UNLOAD, not START STOP UNIT). Power is managed through this page: the
// drive drops to a lower-power state after the matching timer of inactivity,
// and the next command wakes it.
const (
	opModeSelect6 = 0x15
	pagePowerCond = 0x1A
)

// centisecond is the unit of the condition timers: 100 milliseconds.
const centisecond = 100 * time.Millisecond

// PowerCondition is the drive's power management from the power condition
// mode page (0x1A): the idle and standby condition timers, whether each is
// in effect, and whether the drive lets the host change them.
type PowerCondition struct {
	IdleEnabled    bool
	StandbyEnabled bool
	IdleTimer      time.Duration // how long idle before the idle state
	StandbyTimer   time.Duration // how long idle before the lower standby state
	// IdleSettable and StandbySettable come from the changeable mode page:
	// whether the drive allows the host to set that timer. HaveSettable is
	// false if the drive did not answer the changeable query.
	IdleSettable    bool
	StandbySettable bool
	HaveSettable    bool
	Extended        bool // the drive returned the longer SPC-4 format

	mediumType  byte   // echoed back on a write
	devSpecific byte   // echoed back on a write (buffered mode, speed)
	raw         []byte // the current page, for a read-modify-write
}

// ReadPowerCondition reads the power condition mode page and, when the drive
// answers it, the changeable mode page that says which timers may be set. It
// is a drive setting, so it needs no cartridge. ErrUnsupported means the
// drive does not report the page.
func ReadPowerCondition(d Device) (*PowerCondition, error) {
	pc, raw, hdr, err := readPowerPage(d, 0x00) // current values
	if err != nil {
		return nil, err
	}
	pc.raw, pc.mediumType, pc.devSpecific = raw, hdr[1], hdr[2]
	// The changeable mode page marks, with a 1 bit, every field the host may
	// change. If the drive does not answer it, leave settability unknown.
	if chg, _, _, err := readPowerPage(d, 0x01); err == nil {
		pc.IdleSettable = chg.IdleEnabled || chg.IdleTimer != 0
		pc.StandbySettable = chg.StandbyEnabled || chg.StandbyTimer != 0
		pc.HaveSettable = true
	}
	return &pc, nil
}

// readPowerPage reads the power condition mode page at a page control (0 is
// the current values, 1 the changeable mask) and parses it.
func readPowerPage(d Device, control byte) (PowerCondition, []byte, [4]byte, error) {
	buf := make([]byte, 64)
	// MODE SENSE(6), one page, no block descriptors (DBD).
	n, err := d.Do([]byte{opModeSense6, 0x08, control<<6 | pagePowerCond, 0, byte(len(buf)), 0}, DirIn, buf, shortTimeout)
	if err != nil {
		return PowerCondition{}, nil, [4]byte{}, err
	}
	return parsePowerPage(buf[:n])
}

// parsePowerPage parses a MODE SENSE(6) reply holding the power condition
// page: the parsed fields, the raw page bytes and the 4-byte mode header.
func parsePowerPage(b []byte) (pc PowerCondition, raw []byte, hdr [4]byte, err error) {
	if len(b) < 4 {
		return pc, nil, hdr, errShort
	}
	copy(hdr[:], b[:4])
	// Skip the 4-byte mode header and any block descriptors it declares.
	off := 4 + int(b[3])
	if off+12 > len(b) || b[off]&0x3F != pagePowerCond {
		return pc, nil, hdr, errors.New("the drive did not return the power condition mode page")
	}
	plen := int(b[off+1])
	pc = PowerCondition{
		IdleEnabled:    b[off+3]&0x02 != 0,
		StandbyEnabled: b[off+3]&0x01 != 0,
		IdleTimer:      time.Duration(binary.BigEndian.Uint32(b[off+4:off+8])) * centisecond,
		StandbyTimer:   time.Duration(binary.BigEndian.Uint32(b[off+8:off+12])) * centisecond,
		Extended:       plen >= 0x26,
	}
	raw = append([]byte(nil), b[off:min(off+2+plen, len(b))]...)
	return pc, raw, hdr, nil
}

// SetPowerCondition writes the idle and standby timers and enable bits back
// to the drive with MODE SELECT, keeping the rest of the page and the header
// as they were read. Call ReadPowerCondition first, change the fields, then
// pass it here. A timer the drive does not allow setting is refused with a
// clear error instead of the drive's raw rejection.
func SetPowerCondition(d Device, pc *PowerCondition) error {
	if len(pc.raw) < 12 {
		return errors.New("no power condition page to write back; read it first")
	}
	if pc.HaveSettable {
		switch {
		case !pc.IdleSettable && !pc.StandbySettable:
			return errors.New("this drive manages power itself; its timers cannot be set from the host")
		case pc.IdleEnabled && !pc.IdleSettable:
			return errors.New("this drive does not allow setting the idle power timer (try --standby-after instead)")
		case pc.StandbyEnabled && !pc.StandbySettable:
			return errors.New("this drive does not allow setting the standby power timer (try --idle-after instead)")
		}
	}
	page := append([]byte(nil), pc.raw...)
	page[0] &^= 0x80 // PS is only valid in a MODE SENSE reply
	page[3] &^= 0x03
	if pc.IdleEnabled {
		page[3] |= 0x02
	}
	if pc.StandbyEnabled {
		page[3] |= 0x01
	}
	binary.BigEndian.PutUint32(page[4:8], centiCap(pc.IdleTimer))
	binary.BigEndian.PutUint32(page[8:12], centiCap(pc.StandbyTimer))
	// MODE SELECT(6): the mode header, then the page. Echo the medium type
	// and the device-specific byte (buffered mode and speed) from the read,
	// clearing the write-protect bit that is reserved on a write, so those
	// settings are preserved; mode data length and block descriptors are 0.
	params := append([]byte{0, pc.mediumType, pc.devSpecific &^ 0x80, 0}, page...)
	cdb := []byte{opModeSelect6, 0x10, 0, 0, byte(len(params)), 0} // PF set, SP clear (current values)
	_, err := d.Do(cdb, DirOut, params, shortTimeout)
	return err
}

// centiCap converts a duration to 100 ms units, flooring at zero and capping
// at the 32-bit field so a huge value cannot wrap.
func centiCap(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	cs := d / centisecond
	if cs > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(cs)
}
