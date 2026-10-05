package drive

import (
	"math"
	"testing"
)

func u32p(code uint16, v uint32) LogParam {
	return LogParam{Code: code, Value: []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}}
}

func TestParseDriveStats(t *testing.T) {
	hours := []byte{
		0, 0, 0x5a, 0x68, 0, 0, 0x13, 0x88, // LTO-6 data, 5000 h
		0, 0, 0x5a, 0x6c, 0, 0, 0, 2, // LTO-6 WORM, 2 h
		0, 0, 0x58, 0x58, 0, 0, 0, 0, // LTO-5, never used: skipped
		0, 0, 0x99, 0x10, 0, 0, 0, 1, // unknown density
		0, 0, 0x5a, // torn descriptor: ignored
	}
	lp := LogPage{u32p(0, 1000), u32p(2, 40000), u32p(0x0f, 1), {Code: 0x81, Value: []byte{1}}, {Code: 0x1000, Value: hours}}
	s := parseDriveStats(lp)
	if s.Loads != (Count{1000, true}) || s.PowerOnHours.N != 40000 || s.HardReadErrors.N != 1 || !s.TemperatureExceeded {
		t.Fatalf("%+v", s)
	}
	if s.Cleanings.OK || s.HardWriteErrors.OK {
		t.Fatal("missing counters reported")
	}
	want := []FormatHours{{"LTO-6", 5000}, {"LTO-6 WORM", 2}, {"density 0x99", 1}}
	if len(s.HeadHoursByFormat) != len(want) {
		t.Fatalf("%+v", s.HeadHoursByFormat)
	}
	for i := range want {
		if s.HeadHoursByFormat[i] != want[i] {
			t.Fatalf("%+v", s.HeadHoursByFormat)
		}
	}
}

func TestParseCompression(t *testing.T) {
	lp := LogPage{u32p(0, 120), u32p(1, 250), u32p(6, 3), u32p(7, 6000), u32p(8, 0), u32p(9, 2400), {Code: 0x100, Value: []byte{1}}}
	c := parseCompression(lp)
	if c.WriteRatio.N != 250 || c.ReadRatio.N != 120 || c.Enabled.N != 1 || c.FromHost.N != 3_006_000 || c.ToTape.N != 2400 {
		t.Fatalf("%+v", c)
	}
	if c.ToHost.OK || c.FromTape.OK {
		t.Fatal("missing counters reported")
	}
	// Megabyte counters near the limit saturate instead of overflowing.
	big := LogPage{{Code: 6, Value: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}, u32p(7, 5)}
	if got := bytesCount(big, 6, 7); got.N != math.MaxUint64 || !got.OK {
		t.Fatalf("%+v", got)
	}
}

func TestParseVolumeStats(t *testing.T) {
	if parseVolumeStats(LogPage{u32p(0x16, 1)}) != nil {
		t.Fatal("stats without mounts or retries")
	}
	s := parseVolumeStats(LogPage{u32p(1, 3), u32p(0x101, 40), u32p(0x0e, 20), u32p(0x16, 2500000), u32p(0x17, 10), {Code: 0x82, Value: []byte{1}}})
	if s.Mounts.N != 3 || s.Passes.N != 40 || s.LastMountMBWritten.N != 20 || s.UsedNativeMB.N != 10 || !s.TemperatureExceeded || s.ReadRetries.OK {
		t.Fatalf("%+v", s)
	}
}

func TestParseFirmwareBuild(t *testing.T) {
	good := append([]byte{1, 0xC0, 0, 0x27}, "LTO6_E6R3   130808\x0020140808sas_hh      "...)
	if fb, err := parseFirmwareBuild(good); err != nil || fb.Built != "2014-08-08 13:08:08" {
		t.Fatalf("%+v %v", fb, err)
	}
	if _, err := parseFirmwareBuild(good[:40]); err == nil {
		t.Fatal("short page accepted")
	}
	wrong := append([]byte(nil), good...)
	wrong[1] = 0xC1
	if _, err := parseFirmwareBuild(wrong); err == nil {
		t.Fatal("wrong page accepted")
	}
	// Garbage instead of a date leaves the date out.
	odd := append([]byte(nil), good...)
	copy(odd[23:], "2014XX08")
	if fb, err := parseFirmwareBuild(odd); err != nil || fb.Built != "" || fb.Name != "LTO6_E6R3" {
		t.Fatalf("%+v %v", fb, err)
	}
}
