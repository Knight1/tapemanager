// Package drivetest provides a fake tape drive for tests. Its responses are
// modeled on an IBM ULT3580-HH6 (LTO-6) with an LTFS cartridge loaded.
package drivetest

import (
	"encoding/binary"
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
	VolumeReadErrs uint64 // unrecovered read errors over the cartridge's life
	PreventRemoval bool   // unload fails like a drive locked by LTFS
	WriteProtect   bool   // cartridge write protected
	DeviceType     byte   // INQUIRY device type, 1 (tape) unless set
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
	return &Fake{DevPath: "/dev/sg9", DeviceType: 1, LTFSUUID: "608239c8-5f70-457c-99d4-8a4608c38a4e"}
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
			copy(resp[8:], "IBM     ULT3580-HH6     E6R3")
		} else if cdb[2] == 0x80 {
			resp = append([]byte{1, 0x80, 0, 10}, "1068035960"...)
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
		return page(code, param(1, u32(2)), param(3, u32(1)), param(4, u32(0)), param(8, u32(0)), param(9, u32(f.VolumeReadErrs)))
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
			attr(0x0000, 0, u64(35050)),
			attr(0x0001, 0, u64(35060)),
			attr(0x0003, 0, u64(2)),
			attr(0x0220, 0, u64(18)),
			attr(0x0221, 0, u64(25)),
			attr(0x0400, 1, pad("QUANTUM", 8)),
			attr(0x0401, 1, pad("6220913053", 32)),
			attr(0x0405, 0, []byte{0x5A}),
			attr(0x0406, 1, []byte("20220913")),
			attr(0x0408, 0, []byte{0}),
			attr(0x0800, 1, pad("IBM", 8)),
			attr(0x0801, 1, pad("LTFS", 32)),
			attr(0x0802, 1, pad("2.4.9.0", 8)),
			attr(0x0806, 1, pad("", 32)),
			attr(0x080C, 0, coherency),
		)
	} else {
		list = append(list, attr(0x0000, 0, u64(2314049)), attr(0x0001, 0, u64(2314062)))
	}
	b := make([]byte, 4)
	for _, a := range list {
		b = append(b, a...)
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)-4))
	return b
}
