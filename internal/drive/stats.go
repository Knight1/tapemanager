package drive

import (
	"encoding/binary"
	"math"
)

// More log pages (SSC-4).
const (
	PageNonMediumErrors = 0x06
	PageSequential      = 0x0C
	PageDeviceStats     = 0x14
	PageCompression     = 0x1B
)

// Count is a counter the drive may or may not report.
type Count struct {
	N  uint64
	OK bool
}

func count(lp LogPage, code uint16) Count {
	v, ok := lp.Uint(code)
	return Count{v, ok}
}

// DriveStats are the drive's lifetime statistics (device statistics log
// page 0x14). They belong to the drive, not to any cartridge.
type DriveStats struct {
	Loads               Count // cartridge loads over the drive's life
	Cleanings           Count
	PowerOnHours        Count
	HeadHours           Count // hours with tape moving
	MetersOfTape        Count // tape that passed the head
	HoursSinceCleaning  Count // tape motion hours since the last successful cleaning
	PowerCycles         Count
	HardWriteErrors     Count // over the drive's life
	HardReadErrors      Count
	TemperatureExceeded bool // maximum recommended mechanism temperature exceeded
	// HeadHoursByFormat lists tape motion hours per cartridge type, for
	// types the drive has used.
	HeadHoursByFormat []FormatHours
}

// FormatHours is the tape motion time for one cartridge type.
type FormatHours struct {
	Format string
	Hours  uint64
}

func parseDriveStats(lp LogPage) *DriveStats {
	s := &DriveStats{
		Loads:              count(lp, 0x0000),
		Cleanings:          count(lp, 0x0001),
		PowerOnHours:       count(lp, 0x0002),
		HeadHours:          count(lp, 0x0003),
		MetersOfTape:       count(lp, 0x0004),
		HoursSinceCleaning: count(lp, 0x0008),
		PowerCycles:        count(lp, 0x000C),
		HardWriteErrors:    count(lp, 0x000E),
		HardReadErrors:     count(lp, 0x000F),
	}
	if v, ok := lp.Uint(0x0081); ok && v != 0 {
		s.TemperatureExceeded = true
	}
	// Descriptors of 8 bytes: 2 reserved, density code, medium type,
	// 4 bytes of hours.
	if p, ok := lp.Get(0x1000); ok {
		for off := 0; off+8 <= len(p.Value); off += 8 {
			d := p.Value[off : off+8]
			h := uint64(binary.BigEndian.Uint32(d[4:]))
			if h == 0 {
				continue
			}
			name := densityNames[d[2]]
			if name == "" {
				name = "density 0x" + hexByte(d[2])
			}
			if d[3]&0x04 != 0 { // WORM variants have this bit set in the medium type
				name += " WORM"
			}
			s.HeadHoursByFormat = append(s.HeadHoursByFormat, FormatHours{name, h})
		}
	}
	return s
}

func hexByte(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0xF]})
}

// Compression is the data compression log page (0x1B): what the drive
// compressed since the cartridge was loaded.
type Compression struct {
	Enabled    Count // 1 if the drive compresses
	ReadRatio  Count // x100, 250 means 2.5:1
	WriteRatio Count
	FromHost   Count // bytes the host wrote
	ToTape     Count // bytes that went to tape after compression
	FromTape   Count
	ToHost     Count
}

// bytesCount combines a megabyte counter and its remainder in bytes.
func bytesCount(lp LogPage, mb, b uint16) Count {
	m, ok1 := lp.Uint(mb)
	r, ok2 := lp.Uint(b)
	if !ok1 || !ok2 {
		return Count{}
	}
	if m > (math.MaxUint64-r)/1_000_000 {
		return Count{math.MaxUint64, true}
	}
	return Count{m*1_000_000 + r, true}
}

func parseCompression(lp LogPage) *Compression {
	return &Compression{
		Enabled:    count(lp, 0x0100),
		ReadRatio:  count(lp, 0x0000),
		WriteRatio: count(lp, 0x0001),
		ToHost:     bytesCount(lp, 0x0002, 0x0003),
		FromTape:   bytesCount(lp, 0x0004, 0x0005),
		FromHost:   bytesCount(lp, 0x0006, 0x0007),
		ToTape:     bytesCount(lp, 0x0008, 0x0009),
	}
}

// VolumeStats are what the drive reports about the loaded cartridge
// (volume statistics log page 0x17).
type VolumeStats struct {
	Mounts           Count // over the cartridge's life
	Passes           Count // full passes from the beginning of the tape
	WriteRetries     Count // over the cartridge's life
	WriteUnrecovered Count
	ReadRetries      Count
	ReadUnrecovered  Count
	// Unrecovered errors and MB (10^6 bytes) during the last mount.
	LastMountWriteUnrecovered Count
	LastMountReadUnrecovered  Count
	LastMountMBWritten        Count
	LastMountMBRead           Count
	NativeCapacityMB          Count // whole cartridge, without compression
	UsedNativeMB              Count
	TemperatureExceeded       bool // tape path temperature exceeded
}

func parseVolumeStats(lp LogPage) *VolumeStats {
	if _, ok := lp.Get(0x0001); !ok {
		if _, ok := lp.Get(0x0003); !ok {
			return nil
		}
	}
	s := &VolumeStats{
		Mounts:                    count(lp, 0x0001),
		Passes:                    count(lp, 0x0101),
		WriteRetries:              count(lp, 0x0003),
		WriteUnrecovered:          count(lp, 0x0004),
		ReadRetries:               count(lp, 0x0008),
		ReadUnrecovered:           count(lp, 0x0009),
		LastMountWriteUnrecovered: count(lp, 0x000C),
		LastMountReadUnrecovered:  count(lp, 0x000D),
		LastMountMBWritten:        count(lp, 0x000E),
		LastMountMBRead:           count(lp, 0x000F),
		NativeCapacityMB:          count(lp, 0x0016),
		UsedNativeMB:              count(lp, 0x0017),
	}
	if v, ok := lp.Uint(0x0082); ok && v != 0 {
		s.TemperatureExceeded = true
	}
	return s
}
