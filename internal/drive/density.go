package drive

import (
	"encoding/binary"
	"errors"
)

// REPORT DENSITY SUPPORT and the data compression mode page.
const (
	opReportDensity = 0x44
	pageCompression = 0x0F // data compression mode page (MODE SENSE 0x1A)
)

// Density is one recording format the drive supports, from REPORT DENSITY
// SUPPORT.
type Density struct {
	PrimaryCode   byte
	SecondaryCode byte
	Writable      bool   // WRTOK: the drive can write this density, not only read it
	Duplicate     bool   // DUP: more than one descriptor shares this density code
	Default       bool   // DEFLT: the drive's default density
	CapacityMB    uint32 // nominal native capacity, in millions of bytes
	Organization  string // assigning organization
	Name          string // the drive's short name for the density
	Description   string // the drive's description of the density
	Generation    string // LTO generation from the density code, if known
}

const densityDescriptor = 52 // fixed descriptor length (SSC-4)

// ReportDensitySupport lists the recording formats the drive supports. It
// asks for the drive's own densities, not a medium's, so it needs no
// cartridge.
func ReportDensitySupport(d Device) ([]Density, error) {
	// Room for more descriptors than any LTO drive reports; a short transfer
	// is parsed for what arrived.
	buf := make([]byte, 4+densityDescriptor*32)
	cdb := make([]byte, 10)
	cdb[0] = opReportDensity
	binary.BigEndian.PutUint16(cdb[7:], uint16(len(buf)))
	n, err := d.Do(cdb, DirIn, buf, shortTimeout)
	if err != nil {
		return nil, err
	}
	return parseDensitySupport(buf[:n]), nil
}

func parseDensitySupport(b []byte) []Density {
	if len(b) < 4 {
		return nil
	}
	end := min(4+int(binary.BigEndian.Uint16(b)), len(b))
	var out []Density
	for off := 4; off+densityDescriptor <= end; off += densityDescriptor {
		d := b[off : off+densityDescriptor]
		out = append(out, Density{
			PrimaryCode:   d[0],
			SecondaryCode: d[1],
			Writable:      d[2]&0x80 != 0,
			Duplicate:     d[2]&0x40 != 0,
			Default:       d[2]&0x20 != 0,
			CapacityMB:    binary.BigEndian.Uint32(d[12:16]),
			Organization:  printable(d[16:24]),
			Name:          printable(d[24:32]),
			Description:   printable(d[32:52]),
			Generation:    densityNames[d[0]],
		})
	}
	return out
}

// CompressionEnabled reports whether the drive's hardware data compression is
// turned on, from the data compression mode page (0x0F). It is a drive
// setting, so it needs no cartridge. ok is false when the drive does not
// report that page.
func CompressionEnabled(d Device) (enabled, ok bool, err error) {
	buf := make([]byte, 32)
	// MODE SENSE(6), one page, no block descriptors (DBD).
	n, err := d.Do([]byte{opModeSense6, 0x08, pageCompression, 0, byte(len(buf)), 0}, DirIn, buf, shortTimeout)
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			return false, false, nil
		}
		return false, false, err
	}
	b := buf[:n]
	if len(b) < 4 {
		return false, false, errShort
	}
	// Skip the 4-byte mode header and any block descriptors it declares.
	off := 4 + int(b[3])
	if off+3 > len(b) || b[off]&0x3F != pageCompression {
		return false, false, nil
	}
	// Page byte 2, bit 7 (DCE) is data compression enable.
	return b[off+2]&0x80 != 0, true, nil
}
