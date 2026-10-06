package drive

import "testing"

func TestParseMicrocodeBuffer(t *testing.T) {
	for _, c := range []struct {
		raw      byte
		boundary int
	}{
		{0, 0},
		{9, 9},
		{23, 23},
		{0xFF, 0}, // no alignment
		{24, BoundaryUndefined},
		{0x86, BoundaryUndefined}, // IBM LTO-6 firmware H991
		{0xFE, BoundaryUndefined},
	} {
		m := parseMicrocodeBuffer([]byte{c.raw, 0x50, 0, 0})
		if m.Boundary != c.boundary || m.Raw != c.raw || m.Capacity != 5<<20 {
			t.Errorf("%#x: %+v", c.raw, m)
		}
	}
}

func TestCheckFirmwareImageUndefinedBoundary(t *testing.T) {
	b := MicrocodeBuffer{Boundary: BoundaryUndefined}
	for _, in := range []int{0, 1000, DefaultFirmwareChunk, 3 * DefaultFirmwareChunk} {
		c, err := CheckFirmwareImage(b, make([]byte, 10), in)
		if err != nil || c <= 0 || c%DefaultFirmwareChunk != 0 {
			t.Errorf("chunk %d: %d %v", in, c, err)
		}
	}
	if _, err := CheckFirmwareImage(MicrocodeBuffer{Boundary: -2}, make([]byte, 10), 0); err == nil {
		t.Error("negative boundary accepted")
	}
}
