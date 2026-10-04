package drive

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Responses captured from an IBM ULT3580-HH6 (LTO-6, firmware E6R3).
const (
	realWriteErrors = `02 00 00 4c 00 00 40 04 00 00 00 00 00 01 40 04
		00 00 00 00 00 02 40 04 00 00 00 00 00 03 40 04 00 00 00 01 00 04 40 04
		00 00 00 01 00 05 40 04 00 00 00 06 00 06 40 04 00 00 00 00 80 00 40 08
		00 00 00 00 00 00 00 00 80 01 40 04 00 00 00 00`
	realDeviceStatus = `11 00 00 2c 00 00 43 04 b1 17 00 02 00 01 43 02 01 f4
		00 02 43 08 00 00 00 00 00 00 00 00 00 03 43 0c 00 00 00 00 00 00 00 00
		00 00 00 00`
	realCoherency = `08 00 00 00 00 00 00 00 09 00 00 00 00 00 00 00 01 00 00
		00 00 00 00 00 05 00 2b 4c 54 46 53 c0 36 30 38 32 33 39 63 38 2d 35 66
		37 30 2d 34 35 37 63 2d 39 39 64 34 2d 38 61 34 36 30 38 63 33 38 61 34
		65 00 01`
)

func TestParseInquiry(t *testing.T) {
	b := make([]byte, 70)
	b[0] = 0x01
	copy(b[8:], "IBM     ULT3580-HH6     E6R3")
	q, err := parseInquiry(b)
	if err != nil || !q.IsTape() || q.Vendor != "IBM" || q.Product != "ULT3580-HH6" || q.Revision != "E6R3" {
		t.Fatalf("%+v %v", q, err)
	}
	if _, err := parseInquiry(b[:35]); !errors.Is(err, errShort) {
		t.Fatalf("short inquiry: %v", err)
	}
	b[0] = 0x00
	if q, _ := parseInquiry(b); q.IsTape() {
		t.Fatal("disk reported as tape")
	}
}

func TestParseSerial(t *testing.T) {
	b := append([]byte{1, 0x80, 0, 10}, "1068035960"...)
	if s, err := parseSerial(b); err != nil || s != "1068035960" {
		t.Fatalf("%q %v", s, err)
	}
	// A page length larger than the data is cut to what was received.
	b[3] = 200
	if s, err := parseSerial(b); err != nil || s != "1068035960" {
		t.Fatalf("%q %v", s, err)
	}
	if _, err := parseSerial([]byte{1, 0x83, 0, 0}); err == nil {
		t.Fatal("wrong page accepted")
	}
	if _, err := parseSerial([]byte{1}); err == nil {
		t.Fatal("short page accepted")
	}
	// Control characters from the drive are dropped.
	if s, _ := parseSerial([]byte{1, 0x80, 0, 4, 'A', 0x1b, '[', 0}); s != "A[" {
		t.Fatalf("%q", s)
	}
}

func TestParseLogPageReal(t *testing.T) {
	lp, err := parseLogPage(unhex(t, realWriteErrors), PageWriteErrors)
	if err != nil {
		t.Fatal(err)
	}
	if len(lp) != 9 {
		t.Fatalf("%d params", len(lp))
	}
	c := parseErrorCounters(lp)
	if c.Corrected != 1 || c.Uncorrected != 0 || c.Bytes != 6 || !c.HaveCorrected || !c.HaveUncorr {
		t.Fatalf("%+v", c)
	}
	if _, ok := lp.Uint(0x7777); ok {
		t.Fatal("missing parameter found")
	}

	lp, err = parseLogPage(unhex(t, realDeviceStatus), PageDeviceStatus)
	if err != nil {
		t.Fatal(err)
	}
	v, err := parseVHF(lp)
	if err != nil {
		t.Fatal(err)
	}
	want := VHF{Compression: true, MediumPresent: true, Threaded: true, Mounted: true}
	if v != want || v.ActivityName() != "idle" {
		t.Fatalf("%+v", v)
	}
}

func TestParseLogPageUntrusted(t *testing.T) {
	if _, err := parseLogPage([]byte{2, 0}, 2); !errors.Is(err, errShort) {
		t.Fatalf("short: %v", err)
	}
	if _, err := parseLogPage([]byte{3, 0, 0, 0}, 2); err == nil {
		t.Fatal("wrong page accepted")
	}
	// Page length claims more than received, last parameter torn.
	b := []byte{2, 0, 0xFF, 0xFF, 0, 1, 0x40, 2, 0, 5, 0, 2, 0x40, 8, 1}
	lp, err := parseLogPage(b, 2)
	if err != nil || len(lp) != 1 {
		t.Fatalf("%v %v", lp, err)
	}
	if v, ok := lp.Uint(1); !ok || v != 5 {
		t.Fatalf("%d %v", v, ok)
	}
	// Page length shorter than the data: the rest is ignored.
	b = []byte{2, 0, 0, 0, 0, 1, 0x40, 2, 0, 5}
	if lp, _ := parseLogPage(b, 2); len(lp) != 0 {
		t.Fatalf("%v", lp)
	}
	// Values longer than 8 bytes or empty are not numbers.
	if _, ok := (LogParam{Value: make([]byte, 9)}).Uint(); ok {
		t.Fatal("9-byte value accepted")
	}
	if _, ok := (LogParam{}).Uint(); ok {
		t.Fatal("empty value accepted")
	}
}

func TestParseVHFBits(t *testing.T) {
	page := func(b0, b1, b2 byte) LogPage {
		return LogPage{{Code: 0, Value: []byte{b0, b1, b2, 0}}}
	}
	v, _ := parseVHF(page(0x04, 0, 0x06))
	if !v.CleanRequested || v.CleanRequired || v.ActivityName() != "writing" {
		t.Fatalf("%+v", v)
	}
	v, _ = parseVHF(page(0x02|0x08, 0x80, 0x55))
	if !v.CleanRequired || !v.WriteProtect || !v.InTransition || v.ActivityName() != "activity 0x55" {
		t.Fatalf("%+v", v)
	}
	if _, err := parseVHF(LogPage{{Code: 0, Value: []byte{1, 2}}}); err == nil {
		t.Fatal("short VHF accepted")
	}
	if _, err := parseVHF(nil); err == nil {
		t.Fatal("missing VHF accepted")
	}
}

func TestParseTapeAlerts(t *testing.T) {
	lp := LogPage{
		{Code: 1, Value: []byte{0}},
		{Code: 20, Value: []byte{1}},
		{Code: 60, Value: []byte{0x81}}, // unknown flag, set
		{Code: 65, Value: []byte{1}},    // outside the flag range
		{Code: 2, Value: nil},           // malformed
		{Code: 10, Value: []byte{1}},
	}
	got := parseTapeAlerts(lp)
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	if got[0].Flag != 20 || got[0].Severity != SevCritical || !strings.Contains(got[0].Name, "clean now") {
		t.Fatalf("%+v", got[0])
	}
	if got[1].Flag != 60 || got[1].Name != "TapeAlert flag 60" || got[1].Severity != SevWarning {
		t.Fatalf("%+v", got[1])
	}
	if got[2].Severity != SevInfo || got[2].Severity.String() != "info" || SevCritical.String() != "critical" || SevWarning.String() != "warning" {
		t.Fatalf("%+v", got[2])
	}
}

func TestParseAttributes(t *testing.T) {
	b := []byte{0, 0, 0, 0,
		0x04, 0x01, 0x81, 0, 4, 'S', 'N', ' ', ' ',
		0x00, 0x00, 0x00, 0, 8, 0, 0, 0, 0, 0, 0, 0x88, 0xea,
		0x08, 0x06, 0x01, 0, 50, 'x'} // torn: claims 50 bytes
	b[3] = byte(len(b) - 4)
	a, err := parseAttributes(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 2 || a.String(AttrSerial) != "SN" || !a[AttrSerial].ReadOnly || a[AttrSerial].Format != 1 {
		t.Fatalf("%+v", a)
	}
	if v, ok := a.Uint(AttrRemainingCapacity); !ok || v != 35050 {
		t.Fatalf("%d %v", v, ok)
	}
	if a.String(AttrBarcode) != "" {
		t.Fatal("torn attribute kept")
	}
	if _, ok := a.Uint(AttrBarcode); ok {
		t.Fatal("missing attribute found")
	}
	// An available length beyond the buffer is cut to what was received.
	b[0], b[1], b[2], b[3] = 0xFF, 0xFF, 0xFF, 0xFF
	if a, err := parseAttributes(b); err != nil || len(a) != 2 {
		t.Fatalf("%v %v", a, err)
	}
	if _, err := parseAttributes([]byte{0, 0}); !errors.Is(err, errShort) {
		t.Fatalf("short: %v", err)
	}
}

func TestParseCoherencyUUID(t *testing.T) {
	b := unhex(t, realCoherency)
	if u := parseCoherencyUUID(b); u != "608239c8-5f70-457c-99d4-8a4608c38a4e" {
		t.Fatalf("%q", u)
	}
	bad := map[string][]byte{
		"empty":       nil,
		"no acsi":     b[:20],
		"acsi length": append(append([]byte(nil), b[:25]...), 0xFF, 0xFF),
		"short acsi":  append(append([]byte(nil), b[:25]...), 0, 4, 'L', 'T', 'F', 'S'),
		"huge vcr":    {0xFF, 1, 2},
	}
	notLTFS := append([]byte(nil), b...)
	copy(notLTFS[27:], "XXXX")
	bad["not ltfs"] = notLTFS
	badUUID := append([]byte(nil), b...)
	badUUID[33] = '/'
	bad["bad uuid"] = badUUID
	for name, v := range bad {
		if u := parseCoherencyUUID(v); u != "" {
			t.Errorf("%s: %q", name, u)
		}
	}
}

func TestCommandError(t *testing.T) {
	fixed := make([]byte, 18)
	fixed[0], fixed[2], fixed[12], fixed[13] = 0x70, 0x02, 0x3A, 0x00
	e := newCommandError(0x00, 0x02, fixed)
	if !errors.Is(e, ErrNoMedium) || errors.Is(e, ErrUnsupported) || !strings.Contains(e.Error(), "no cartridge") {
		t.Fatalf("%v", e)
	}
	desc := []byte{0x72, 0x05, 0x53, 0x02, 0, 0, 0, 0}
	e = newCommandError(0x1B, 0x02, desc)
	if !errors.Is(e, ErrRemovalPrevented) || !strings.Contains(e.Error(), "illegal request: medium removal prevented") {
		t.Fatalf("%v", e)
	}
	e = newCommandError(0x4D, 0x02, []byte{0x72, 0x05, 0x24, 0})
	if !errors.Is(e, ErrUnsupported) {
		t.Fatalf("%v", e)
	}
	e = newCommandError(0x4D, 0x02, []byte{0x70, 0, 0x04, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x99, 0x01})
	if !strings.Contains(e.Error(), "hardware error (ASC 0x99, ASCQ 0x01)") {
		t.Fatalf("%v", e)
	}
	// Truncated or missing sense data does not panic.
	for _, s := range [][]byte{nil, {0x70}, {0x70, 0, 3}, {0x72, 1}} {
		_ = newCommandError(1, 2, s).Error()
	}
	if e := newCommandError(0x12, 0x18, nil); !strings.Contains(e.Error(), "SCSI status 0x18") {
		t.Fatalf("%v", e)
	}
}

func TestCDBs(t *testing.T) {
	if c := inquiryCDB(true, 0x80, 255); c[0] != 0x12 || c[1] != 1 || c[2] != 0x80 || c[4] != 255 {
		t.Fatalf("% x", c)
	}
	if c := logSenseCDB(0x2E, 0x1000); c[0] != 0x4D || c[2] != 0x6E || c[7] != 0x10 || c[8] != 0 {
		t.Fatalf("% x", c)
	}
	if c := readAttributeCDB(1, 0x0401, 0x10000); len(c) != 16 || c[7] != 1 || c[8] != 4 || c[9] != 1 || c[11] != 1 {
		t.Fatalf("% x", c)
	}
	if c := loadUnloadCDB(false); c[0] != 0x1B || c[4] != 0 {
		t.Fatalf("% x", c)
	}
	if c := loadUnloadCDB(true); c[4] != 1 {
		t.Fatalf("% x", c)
	}
}

func FuzzParseLogPage(f *testing.F) {
	f.Add(unhex(f, realWriteErrors))
	f.Add(unhex(f, realDeviceStatus))
	f.Fuzz(func(t *testing.T, b []byte) {
		lp, err := parseLogPage(b, 0x02)
		if err != nil {
			return
		}
		parseErrorCounters(lp)
		parseVHF(lp)
		parseTapeAlerts(lp)
		parseVolumeErrors(lp)
	})
}

func FuzzParseAttributes(f *testing.F) {
	f.Add([]byte{0, 0, 0, 9, 0x08, 0x0c, 0, 0, 4, 1, 2, 3, 4})
	f.Add(unhex(f, realCoherency))
	f.Fuzz(func(t *testing.T, b []byte) {
		parseCoherencyUUID(b)
		a, err := parseAttributes(b)
		if err != nil {
			return
		}
		for id := range a {
			a.String(id)
			a.Uint(id)
		}
	})
}
