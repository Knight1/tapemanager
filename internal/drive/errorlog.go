package drive

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PageDiagnostics is the tape diagnostic data log page: the drive's own
// history of failed operations. Drives keep a fixed number of slots;
// unused slots are all zero.
const PageDiagnostics = 0x16

// LogEntry is one entry of the drive's error history.
type LogEntry struct {
	Slot        int    // parameter code, the drive's slot number
	Format      string // cartridge type at the time, from density and medium type
	Repeated    bool   // the same error occurred more than once
	Key         byte   // sense key
	ASC, ASCQ   byte
	VendorCode  uint32 // drive specific error code, for the vendor's service
	Firmware    string // firmware level when it happened
	CleanHours  uint32 // tape motion hours since the last cleaning
	Op          byte   // SCSI command that failed
	MediumID    string // the cartridge it happened with
	Origin      byte   // timestamp origin: 0 power-on, otherwise set by a host
	Timestamp   uint64 // milliseconds
	MotionHours uint32 // lifetime tape motion hours, if the drive fills it in
}

// ReadErrorLog returns the used entries of the drive's error history, in
// the order the drive keeps them, and the number of slots.
func ReadErrorLog(d Device) ([]LogEntry, int, error) {
	lp, err := ReadLogPage(d, PageDiagnostics)
	if err != nil {
		return nil, 0, err
	}
	entries, slots := parseErrorLog(lp)
	return entries, slots, nil
}

func parseErrorLog(lp LogPage) ([]LogEntry, int) {
	var out []LogEntry
	slots := 0
	for _, p := range lp {
		b := p.Value
		if len(b) < 68 {
			continue
		}
		slots++
		e := LogEntry{
			Slot:        int(p.Code),
			MotionHours: binary.BigEndian.Uint32(b[4:]),
			Repeated:    b[9]&0x80 != 0,
			Key:         b[9] & 0x0F,
			ASC:         b[10],
			ASCQ:        b[11],
			VendorCode:  binary.BigEndian.Uint32(b[12:]),
			Firmware:    printable(b[16:20]),
			CleanHours:  binary.BigEndian.Uint32(b[20:]),
			Op:          b[24],
			MediumID:    printable(b[28:60]),
			Origin:      b[60] & 0x07,
		}
		for _, c := range b[62:68] {
			e.Timestamp = e.Timestamp<<8 | uint64(c)
		}
		if e.Key == 0 && e.ASC == 0 && e.ASCQ == 0 && e.MediumID == "" && e.VendorCode == 0 {
			continue // unused slot
		}
		e.Format = densityNames[b[2]]
		if e.Format != "" && b[3]&0x04 != 0 {
			e.Format += " WORM"
		}
		out = append(out, e)
	}
	return out, slots
}

// Description says what went wrong.
func (e LogEntry) Description() string {
	d := senseKeyNames[e.Key&0xF]
	if t := ascText(e.ASC, e.ASCQ); t != "" {
		return d + ": " + t
	}
	return fmt.Sprintf("%s (ASC 0x%02x, ASCQ 0x%02x)", d, e.ASC, e.ASCQ)
}

var opNames = map[byte]string{
	0x00: "test unit ready", 0x01: "rewind", 0x04: "format", 0x08: "read",
	0x0A: "write", 0x10: "write filemarks", 0x11: "space", 0x19: "erase",
	0x1B: "load/unload", 0x2B: "locate", 0x34: "read position", 0x3B: "write buffer",
	0x3C: "read buffer", 0x4D: "log sense", 0x8C: "read attribute", 0x8D: "write attribute",
	0x91: "space", 0x92: "locate", 0xA2: "security protocol in", 0xB5: "security protocol out",
}

// Operation names the command that failed.
func (e LogEntry) Operation() string {
	if n, ok := opNames[e.Op]; ok {
		return n
	}
	return fmt.Sprintf("command 0x%02x", e.Op)
}

// When describes the timestamp. Most drives count from power-on, so such
// times cannot be compared across power cycles.
func (e LogEntry) When() string {
	if e.Timestamp == 0 {
		return ""
	}
	if e.Origin != 0 && e.Timestamp < 1<<47 {
		return time.UnixMilli(int64(e.Timestamp)).UTC().Format("2006-01-02 15:04 UTC")
	}
	d := time.Duration(min(e.Timestamp, 1<<62/uint64(time.Millisecond))) * time.Millisecond
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh after a power-on", days, h)
	}
	return fmt.Sprintf("%dh %dm after a power-on", h, m)
}

// Findings summarizes the error history for the user. loaded is the serial
// of the cartridge in the drive, current the drive's firmware level.
func AnalyzeErrorLog(entries []LogEntry, loaded, current string) []string {
	if len(entries) == 0 {
		return nil
	}
	var out []string
	media := map[string][]LogEntry{}
	var hardware []LogEntry
	older := map[string]int{}
	for _, e := range entries {
		switch e.Key {
		case SenseMediumError:
			id := e.MediumID
			if id == "" {
				id = "unknown cartridge"
			}
			media[id] = append(media[id], e)
		case SenseHardwareError:
			hardware = append(hardware, e)
		}
		if e.Firmware != "" && current != "" && e.Firmware != current {
			older[e.Firmware]++
		}
	}
	ids := make([]string, 0, len(media))
	for id := range media {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		es := media[id]
		var kinds []string
		seen := map[string]bool{}
		for _, e := range es {
			if d := ascText(e.ASC, e.ASCQ); d != "" && !seen[d] {
				seen[d] = true
				kinds = append(kinds, d)
			}
		}
		what := fmt.Sprintf("%d medium error", len(es))
		if len(es) != 1 {
			what += "s"
		}
		if len(kinds) > 0 {
			what += " (" + strings.Join(kinds, ", ") + ")"
		}
		if loaded != "" && id == loaded {
			out = append(out, fmt.Sprintf("the loaded cartridge %s has %s in the drive's log: verify it and copy what you need to another tape", id, what))
		} else {
			out = append(out, fmt.Sprintf("cartridge %s: %s; verify it the next time it is loaded", id, what))
		}
	}
	if len(media) >= 3 {
		out = append(out, fmt.Sprintf("medium errors on %d different cartridges point to the drive (dirty or worn head) rather than the tapes: clean the drive and watch whether new entries appear", len(media)))
	}
	if len(hardware) > 0 {
		out = append(out, fmt.Sprintf("%d hardware error(s): the drive itself reported a fault; if new ones appear, contact the vendor's service with the error codes", len(hardware)))
	}
	if len(older) > 0 {
		var levels []string
		for f := range older {
			levels = append(levels, f)
		}
		sort.Strings(levels)
		n := len(entries) - countCurrent(entries, current)
		out = append(out, fmt.Sprintf("%d entr%s recorded with older firmware (%s), before the current %s",
			n, pluralY(n), strings.Join(levels, ", "), current))
	}
	return out
}

func countCurrent(entries []LogEntry, current string) int {
	n := 0
	for _, e := range entries {
		if e.Firmware == "" || e.Firmware == current {
			n++
		}
	}
	return n
}

func pluralY(n int) string {
	if n == 1 {
		return "y was"
	}
	return "ies were"
}
