// Package drivetest provides a fake tape drive for tests. Its responses are
// modeled on an IBM ULT3580-HH6 (LTO-6) with an LTFS cartridge loaded; all
// identifiers and counters are made up.
package drivetest

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/Knight1/tapemanager/internal/drive"
)

// Fake answers SCSI commands like a real drive.
type Fake struct {
	DevPath        string
	NoMedium       bool
	CleanRequested bool
	CleanRequired  bool
	Alerts         []int  // active TapeAlert flags
	WriteUncorr    uint64 // uncorrected write errors (log page 0x02)
	ReadUncorr     uint64
	VolumeReadErrs uint64   // unrecovered read errors over the cartridge's life
	PreventRemoval bool     // unload fails like a drive locked by LTFS
	WriteProtect   bool     // cartridge write protected
	Encrypting     bool     // drive encryption on, as after an LTFS mount with a key
	KeyID          []byte   // key ID reported while encrypting
	NoEncryption   bool     // drive without encryption support
	ErrorLog       [][]byte // tape diagnostic entries (68 bytes each), 12 slots in total
	NoErrorLog     bool     // drive without the tape diagnostic data page

	// Firmware: the drive reports Revision (E6R3 unless set). A download
	// of ImageSize bytes is activated as NewRevision; Received collects it.
	Vendor         string // IBM unless set
	Revision       string
	ImageSize      int
	NewRevision    string
	BufferCapacity int // 5 MiB unless set
	Boundary       byte
	Received       []byte
	Downloads      int  // WRITE BUFFER commands
	FailAt         int  // fail the WRITE BUFFER at this offset, if > 0
	DeviceType     byte // INQUIRY device type, 1 (tape) unless set
	LTFSUUID       string
	// Fail makes commands with this operation code fail with the given
	// sense key and code.
	Fail     map[byte]*drive.CommandError
	Unloaded int // successful unload commands
	Loaded   int
	Commands []byte // operation codes received
	Closed   bool
}

// New returns a fake drive with a cartridge loaded.
func New() *Fake {
	return &Fake{DevPath: "/dev/sg9", DeviceType: 1, LTFSUUID: "00000000-0000-4000-8000-000000000001"}
}

func (f *Fake) Path() string { return f.DevPath }

func (f *Fake) Close() error { f.Closed = true; return nil }

func check(op, key, asc, ascq byte) error {
	return &drive.CommandError{Op: op, Status: 0x02, Key: key, ASC: asc, ASCQ: ascq}
}

func (f *Fake) Do(cdb []byte, dir drive.Direction, buf []byte, timeout time.Duration) (int, error) {
	op := cdb[0]
	f.Commands = append(f.Commands, op)
	if e, ok := f.Fail[op]; ok {
		return 0, e
	}
	var resp []byte
	switch op {
	case 0x00: // TEST UNIT READY
		if f.NoMedium {
			return 0, check(op, drive.SenseNotReady, 0x3A, 0)
		}
		return 0, nil
	case 0x12: // INQUIRY
		if cdb[1]&1 == 0 {
			resp = make([]byte, 70)
			resp[0] = f.DeviceType
			rev := f.Revision
			if rev == "" {
				rev = "E6R3"
			}
			vendor := f.Vendor
			if vendor == "" {
				vendor = "IBM"
			}
			copy(resp[8:], fmt.Sprintf("%-8s%-16s%s", vendor, "ULT3580-HH6", rev))
		} else if cdb[2] == 0x03 {
			resp = make([]byte, 0x25)
			resp[1], resp[3] = 0x03, 0x21
			copy(resp[8:], f.LoadID())
			copy(resp[12:], "E6R3")
			copy(resp[0x18:], f.ModelID())
		} else if cdb[2] == 0xC0 {
			resp = append([]byte{1, 0xC0, 0, 0x27}, "LTO6_E6R3   130808\x0020140808sas_hh      "...)
		} else if cdb[2] == 0x80 {
			resp = append([]byte{1, 0x80, 0, 10}, "0000000001"...)
		} else {
			return 0, check(op, drive.SenseIllegalRequest, 0x24, 0)
		}
	case 0x4D: // LOG SENSE
		resp = f.logPage(cdb[2] & 0x3F)
		if resp == nil {
			return 0, check(op, drive.SenseIllegalRequest, 0x24, 0)
		}
	case 0x8C: // READ ATTRIBUTE
		if f.NoMedium {
			return 0, check(op, drive.SenseNotReady, 0x3A, 0)
		}
		resp = f.attributes(cdb[7])
	case 0x3C: // READ BUFFER
		if cdb[1] != 0x03 || cdb[2] != 0 {
			return 0, check(op, drive.SenseIllegalRequest, 0x24, 0)
		}
		c := f.BufferCapacity
		if c == 0 {
			c = 5 << 20
		}
		resp = []byte{f.Boundary, byte(c >> 16), byte(c >> 8), byte(c)}
	case 0x3B: // WRITE BUFFER, download microcode with offsets and save
		f.Downloads++
		off := int(cdb[3])<<16 | int(cdb[4])<<8 | int(cdb[5])
		n := int(cdb[6])<<16 | int(cdb[7])<<8 | int(cdb[8])
		if cdb[1] != 0x07 || cdb[2] != 0 || n != len(buf) || off != len(f.Received) || off%(1<<f.Boundary) != 0 {
			return 0, check(op, drive.SenseIllegalRequest, 0x24, 0)
		}
		if f.FailAt > 0 && off >= f.FailAt {
			return 0, check(op, drive.SenseHardwareError, 0x44, 0)
		}
		f.Received = append(f.Received, buf...)
		if len(f.Received) == f.ImageSize && f.NewRevision != "" {
			f.Revision = f.NewRevision
		}
		return 0, nil
	case 0xA2: // SECURITY PROTOCOL IN, tape data encryption
		if f.NoEncryption || cdb[1] != 0x20 {
			return 0, check(op, drive.SenseIllegalRequest, 0x24, 0)
		}
		resp = f.securityPage(binary.BigEndian.Uint16(cdb[2:]))
		if resp == nil {
			return 0, check(op, drive.SenseIllegalRequest, 0x24, 0)
		}
	case 0x1A: // MODE SENSE(6), header only
		if f.NoMedium {
			return 0, check(op, drive.SenseNotReady, 0x3A, 0)
		}
		resp = []byte{3, 0x68, 0x10, 0}
		if f.WriteProtect {
			resp[2] |= 0x80
		}
	case 0x1B: // LOAD UNLOAD
		if cdb[4]&1 == 1 {
			f.Loaded++
			f.NoMedium = false
			return 0, nil
		}
		if f.NoMedium {
			return 0, check(op, drive.SenseNotReady, 0x3A, 0)
		}
		if f.PreventRemoval {
			return 0, check(op, drive.SenseIllegalRequest, 0x53, 0x02)
		}
		f.Unloaded++
		f.NoMedium = true
		return 0, nil
	default:
		return 0, check(op, drive.SenseIllegalRequest, 0x20, 0)
	}
	return copy(buf, resp), nil
}

func param(code uint16, value []byte) []byte {
	p := make([]byte, 4, 4+len(value))
	binary.BigEndian.PutUint16(p, code)
	p[2] = 0x40
	p[3] = byte(len(value))
	return append(p, value...)
}

func u32(v uint64) []byte { return binary.BigEndian.AppendUint32(nil, uint32(v)) }

func page(code byte, params ...[]byte) []byte {
	b := []byte{code, 0, 0, 0}
	for _, p := range params {
		b = append(b, p...)
	}
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)-4))
	return b
}

func (f *Fake) logPage(code byte) []byte {
	switch code {
	case drive.PageWriteErrors:
		return page(code, param(3, u32(1)), param(5, u32(6)), param(6, u32(f.WriteUncorr)))
	case drive.PageReadErrors:
		return page(code, param(3, u32(0)), param(5, u32(2)), param(6, u32(f.ReadUncorr)))
	case drive.PageDeviceStatus:
		vhf := []byte{0xB1, 0x17, 0x00, 0x02}
		if f.NoMedium {
			vhf[1] = 0
		}
		if f.CleanRequested {
			vhf[0] |= 0x04
		}
		if f.CleanRequired {
			vhf[0] |= 0x02
		}
		if f.WriteProtect {
			vhf[0] |= 0x08
		}
		return page(code, param(0, vhf))
	case drive.PageTapeAlert:
		var params [][]byte
		for i := 1; i <= 64; i++ {
			v := byte(0)
			for _, a := range f.Alerts {
				if a == i {
					v = 1
				}
			}
			params = append(params, param(uint16(i), []byte{v}))
		}
		return page(code, params...)
	case drive.PageVolumeStats:
		if f.NoMedium {
			return nil
		}
		return page(code, param(1, u32(3)), param(3, u32(1)), param(4, u32(0)), param(8, u32(0)), param(9, u32(f.VolumeReadErrs)),
			param(0x0c, u32(0)), param(0x0d, u32(0)), param(0x0e, u32(20)), param(0x0f, u32(30)),
			param(0x16, u32(2500000)), param(0x17, u32(10)), param(0x82, []byte{0}), param(0x101, u32(40)))
	case drive.PageDiagnostics:
		if f.NoErrorLog {
			return nil
		}
		var params [][]byte
		for i := range 12 {
			v := make([]byte, 68)
			if i < len(f.ErrorLog) {
				copy(v, f.ErrorLog[i])
			}
			params = append(params, param(uint16(i), v))
		}
		return page(code, params...)
	case drive.PageNonMediumErrors:
		return page(code, param(0, u32(0)))
	case drive.PageSequential:
		v := uint64(0)
		if f.CleanRequired {
			v = 1
		}
		return page(code, param(0, u32(6000)), param(1, u32(2400)), param(0x100, u32(v)))
	case drive.PageDeviceStats: // made-up values
		hours := []byte{0, 0, 0x58, 0x58, 0, 0, 0, 0, 0, 0, 0x5a, 0x68, 0, 0, 0x13, 0x88}
		return page(code, param(0, u32(1000)), param(1, u32(30)), param(2, u32(40000)), param(3, u32(5000)),
			param(4, u32(60000000)), param(8, u32(100)), param(0x0c, u32(20)), param(0x0e, u32(0)),
			param(0x0f, u32(1)), param(0x81, []byte{0}), param(0x1000, hours))
	case drive.PageCompression:
		return page(code, param(0, u32(120)), param(1, u32(250)), param(2, u32(0)), param(3, u32(3000)),
			param(4, u32(0)), param(5, u32(2500)), param(6, u32(0)), param(7, u32(6000)), param(8, u32(0)),
			param(9, u32(2400)), param(0x100, []byte{1}))
	}
	return nil
}

func attr(id uint16, format byte, value []byte) []byte {
	a := make([]byte, 5, 5+len(value))
	binary.BigEndian.PutUint16(a, id)
	a[2] = format
	binary.BigEndian.PutUint16(a[3:], uint16(len(value)))
	return append(a, value...)
}

func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func pad(s string, n int) []byte {
	b := []byte(s)
	for len(b) < n {
		b = append(b, ' ')
	}
	return b
}

func (f *Fake) attributes(partition byte) []byte {
	var list [][]byte
	if partition == 0 {
		coherency := append([]byte{8}, u64(9)...)
		coherency = append(coherency, u64(1)...)
		coherency = append(coherency, u64(5)...)
		acsi := append([]byte("LTFS\xc0"), f.LTFSUUID...)
		acsi = append(acsi, 0, 1)
		coherency = append(coherency, byte(len(acsi)>>8), byte(len(acsi)))
		coherency = append(coherency, acsi...)
		list = append(list,
			attr(0x0000, 0, u64(35000)),
			attr(0x0001, 0, u64(35010)),
			attr(0x0003, 0, u64(3)),
			attr(0x0220, 0, u64(20)),
			attr(0x0221, 0, u64(30)),
			attr(0x0400, 1, pad("QUANTUM", 8)),
			attr(0x0401, 1, pad("0000000002", 32)),
			attr(0x0405, 0, []byte{0x5A}),
			attr(0x0406, 1, []byte("20200101")),
			attr(0x0408, 0, []byte{0}),
			attr(0x0800, 1, pad("IBM", 8)),
			attr(0x0801, 1, pad("LTFS", 32)),
			attr(0x0802, 1, pad("2.4.9.0", 8)),
			attr(0x0806, 1, pad("", 32)),
			attr(0x080C, 0, coherency),
		)
	} else {
		list = append(list, attr(0x0000, 0, u64(2300000)), attr(0x0001, 0, u64(2300010)))
	}
	b := make([]byte, 4)
	for _, a := range list {
		b = append(b, a...)
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)-4))
	return b
}

func (f *Fake) securityPage(code uint16) []byte {
	var body []byte
	switch code {
	case 0x0010: // capabilities: like the IBM LTO-6, three AES-256-GCM entries
		body = make([]byte, 16)
		body[0] = 0x09
		for i, flags := range []byte{0x3a, 0x3a, 0xba} {
			d := []byte{byte(i + 1), 0, 0, 0x14, flags, 0x34, 0, 0x20, 0, 0x0c, 0, 0x20, 0xeb, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0x14}
			body = append(body, d...)
		}
	case 0x0020: // status
		body = make([]byte, 20)
		body[8] = 0x10
		if f.Encrypting {
			body[1], body[2], body[3] = 2, 3, 1
			body[7] = 1
			kad := []byte{1, 0, 0, byte(len(f.KeyID))}
			body = append(body, append(kad, f.KeyID...)...)
		}
	default:
		return nil
	}
	b := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint16(b, code)
	binary.BigEndian.PutUint16(b[2:], uint16(len(body)))
	return append(b, body...)
}

// DiagEntry builds a tape diagnostic data entry like the IBM drive writes.
func DiagEntry(key, asc, ascq byte, repeated bool, firmware, medium string, op byte, ms uint64) []byte {
	b := make([]byte, 68)
	b[2], b[3] = 0x5a, 0x68
	b[9] = key
	if repeated {
		b[9] |= 0x80
	}
	b[10], b[11] = asc, ascq
	binary.BigEndian.PutUint32(b[12:], 0x0badc0de)
	copy(b[16:20], firmware)
	b[24] = op
	copy(b[28:60], pad(medium, 32))
	for i := range 6 {
		b[67-i] = byte(ms >> (8 * i))
	}
	return b
}

// Made-up IBM load and model IDs of the fake drive ("TESTID01" in EBCDIC).
var (
	FakeLoadID  = []byte{0x11, 0x22, 0x33, 0x44}
	FakeModelID = []byte{0xE3, 0xC5, 0xE2, 0xE3, 0xC9, 0xC4, 0xF0, 0xF1}
)

func (f *Fake) LoadID() []byte  { return FakeLoadID }
func (f *Fake) ModelID() []byte { return FakeModelID }

// IBMImage builds a firmware image with an IBM header for the given load
// and model IDs, level and total size. The rest is random filler.
func IBMImage(loadID, modelID []byte, level string, size int, filler func([]byte)) []byte {
	b := make([]byte, size)
	if filler != nil {
		filler(b)
	}
	b[0], b[1], b[2], b[3] = 0x48, 0, 0x03, 0x91
	binary.BigEndian.PutUint32(b[4:], uint32(size))
	copy(b[8:12], loadID)
	copy(b[12:16], level)
	clear(b[16:0x18])
	copy(b[0x18:0x20], modelID)
	copy(b[0x20:0x28], "IBMTpDrv")
	copy(b[0x300:], "2026/01/02")
	return b
}
