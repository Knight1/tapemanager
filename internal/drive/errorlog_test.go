package drive

import (
	"strings"
	"testing"
)

// Two slots of a tape diagnostic data page as an IBM LTO-6 drive writes them,
// with made-up cartridge IDs and codes.
const sampleDiag0 = `00 00 5a 68 00 00 00 00 00 83 52 00 12 34 56 78 45 36 52 33 00 00 00 00
	08 00 00 00 54 45 53 54 4d 45 44 49 41 31 20 20 20 20 20 20 20 20 20 20
	20 20 20 20 20 20 20 20 20 20 20 20 00 00 00 01 06 63 59 c3`
const sampleDiag1 = `00 00 5a 68 00 00 00 00 00 03 11 00 0b ad c0 de 44 38 45 35 00 00 00 00
	08 00 00 00 54 45 53 54 4d 45 44 49 41 32 20 20 20 20 20 20 20 20 20 20
	20 20 20 20 20 20 20 20 20 20 20 20 00 00 00 00 00 13 83 ae`

func TestParseErrorLogReal(t *testing.T) {
	lp := LogPage{
		{Code: 0, Value: unhex(t, sampleDiag0)},
		{Code: 1, Value: unhex(t, sampleDiag1)},
		{Code: 2, Value: make([]byte, 68)},
		{Code: 3, Value: make([]byte, 10)}, // too short: ignored
	}
	entries, slots := parseErrorLog(lp)
	if slots != 3 || len(entries) != 2 {
		t.Fatalf("%d slots, %+v", slots, entries)
	}
	a, b := entries[0], entries[1]
	if a.Slot != 0 || !a.Repeated || a.Key != SenseMediumError || a.ASC != 0x52 || a.Firmware != "E6R3" ||
		a.MediumID != "TESTMEDIA1" || a.Op != 0x08 || a.Format != "LTO-6" || a.VendorCode != 0x12345678 {
		t.Fatalf("%+v", a)
	}
	if a.Description() != "medium error: cartridge fault" || a.Operation() != "read" || a.When() != "50d 22h after a power-on" {
		t.Fatalf("%q %q %q", a.Description(), a.Operation(), a.When())
	}
	if b.Repeated || b.Firmware != "D8E5" || b.Description() != "medium error: unrecovered read error" || b.When() != "0h 21m after a power-on" {
		t.Fatalf("%+v %q", b, b.When())
	}

	f := AnalyzeErrorLog(entries, "0000000002", "E6R3")
	all := strings.Join(f, "\n")
	for _, want := range []string{
		"cartridge TESTMEDIA1: 1 medium error (cartridge fault)",
		"cartridge TESTMEDIA2: 1 medium error (unrecovered read error)",
		"1 entry was recorded with older firmware (D8E5)",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if strings.Contains(all, "point to the drive") || strings.Contains(all, "loaded cartridge") {
		t.Fatalf("%s", all)
	}
}

func TestAnalyzeErrorLog(t *testing.T) {
	if AnalyzeErrorLog(nil, "", "") != nil {
		t.Fatal("findings without entries")
	}
	e := func(key, asc byte, medium, fw string) LogEntry {
		return LogEntry{Key: key, ASC: asc, MediumID: medium, Firmware: fw}
	}
	entries := []LogEntry{
		e(SenseMediumError, 0x11, "A", "X1"), e(SenseMediumError, 0x11, "A", "X1"), e(SenseMediumError, 0x0C, "A", "X1"),
		e(SenseMediumError, 0x11, "B", "X1"), e(SenseMediumError, 0x11, "C", "X1"),
		e(SenseHardwareError, 0x44, "", "X1"), e(SenseMediumError, 0x11, "", "X0"),
	}
	all := strings.Join(AnalyzeErrorLog(entries, "A", "X1"), "\n")
	for _, want := range []string{
		"loaded cartridge A has 3 medium errors (unrecovered read error, write error)",
		"cartridge B: 1 medium error",
		"unknown cartridge: 1 medium error",
		"medium errors on 4 different cartridges point to the drive",
		"1 hardware error(s)",
		"1 entry was recorded with older firmware (X0)",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
}

func TestLogEntryFormatting(t *testing.T) {
	if (LogEntry{}).When() != "" {
		t.Fatal("zero timestamp")
	}
	// A host-set timestamp is an absolute time.
	if got := (LogEntry{Origin: 2, Timestamp: 1767225600000}).When(); got != "2026-01-01 00:00 UTC" {
		t.Fatal(got)
	}
	// A huge power-on timestamp does not overflow.
	if got := (LogEntry{Timestamp: 1<<48 - 1}).When(); !strings.Contains(got, "after a power-on") {
		t.Fatal(got)
	}
	if (LogEntry{Op: 0xEE}).Operation() != "command 0xee" || (LogEntry{Key: 4, ASC: 0x99}).Description() != "hardware error (ASC 0x99, ASCQ 0x00)" {
		t.Fatal("fallback names")
	}
}
