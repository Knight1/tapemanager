package drive

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// The short SPC-3 format, using the exact example bytes from the sg3_utils
// documentation: idle and standby both enabled, 4 s and 30 s.
func TestParsePowerPageShort(t *testing.T) {
	b := []byte{13, 0x01, 0x02, 0, 0x1A, 0x0A, 0x00, 0x03, 0x00, 0x00, 0x00, 0x28, 0x00, 0x00, 0x01, 0x2c}
	pc, raw, hdr, err := parsePowerPage(b)
	if err != nil {
		t.Fatal(err)
	}
	if pc.Extended || !pc.IdleEnabled || !pc.StandbyEnabled || pc.IdleTimer != 4*time.Second || pc.StandbyTimer != 30*time.Second {
		t.Fatalf("%+v", pc)
	}
	if len(raw) != 12 || raw[0] != 0x1A || hdr[1] != 0x01 || hdr[2] != 0x02 {
		t.Fatalf("raw %x hdr %x", raw, hdr)
	}
}

// The longer SPC-4 format (page length 0x26) is parsed the same way for the
// idle and standby fields, and marked extended.
func TestParsePowerPageExtended(t *testing.T) {
	page := make([]byte, 2+0x26)
	page[0], page[1] = 0x1A, 0x26
	page[3] = 0x02 // IDLE_A enabled, STANDBY_Z off
	binary.BigEndian.PutUint32(page[4:], 40)
	binary.BigEndian.PutUint32(page[8:], 300)
	b := append([]byte{byte(3 + len(page)), 0, 0, 0}, page...)
	pc, _, _, err := parsePowerPage(b)
	if err != nil {
		t.Fatal(err)
	}
	if !pc.Extended || !pc.IdleEnabled || pc.StandbyEnabled || pc.IdleTimer != 4*time.Second {
		t.Fatalf("%+v", pc)
	}
}

func TestParsePowerPageBad(t *testing.T) {
	if _, _, _, err := parsePowerPage([]byte{0, 0}); err == nil {
		t.Fatal("short header accepted")
	}
	// A different page returned in place of 0x1A.
	wrong := []byte{13, 0, 0, 0, 0x0F, 0x0A, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if _, _, _, err := parsePowerPage(wrong); err == nil {
		t.Fatal("wrong page accepted")
	}
}

func TestCentiCap(t *testing.T) {
	if centiCap(0) != 0 || centiCap(-time.Second) != 0 {
		t.Fatal("negative or zero should floor to 0")
	}
	if centiCap(time.Second) != 10 {
		t.Fatalf("1s = %d", centiCap(time.Second))
	}
	if centiCap(time.Duration(math.MaxInt64)) != math.MaxUint32 {
		t.Fatal("a huge duration must cap, not wrap")
	}
}
