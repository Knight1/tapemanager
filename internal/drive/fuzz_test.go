package drive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

// Everything below comes from a drive or a firmware file and is untrusted:
// parsing must never panic and never return values outside the input.

func FuzzParseResponses(f *testing.F) {
	f.Add(byte(0x00), byte(0x02), []byte{0x70, 0, 0x02, 0, 0, 0, 0, 10, 0, 0, 0, 0, 0x04, 0x02})
	f.Add(byte(0x8C), byte(0x02), []byte{0x72, 0x03, 0x04, 0x10})
	f.Add(byte(0x12), byte(0x00), append([]byte{0x01, 0, 0, 0}, bytes.Repeat([]byte("A"), 40)...))
	f.Fuzz(func(t *testing.T, op, status byte, b []byte) {
		e := newCommandError(op, status, b)
		_ = e.Error()
		if errors.Is(e, ErrNoMedium) && errors.Is(e, ErrNotLoaded) {
			t.Fatalf("both no medium and not loaded: %+v", e)
		}
		if q, err := parseInquiry(b); err == nil {
			for _, s := range []string{q.Vendor, q.Product, q.Revision} {
				if len(s) > len(b) {
					t.Fatalf("inquiry field longer than the input")
				}
			}
		}
		if s, err := parseSerial(b); err == nil && len(s) > len(b) {
			t.Fatalf("serial longer than the input")
		}
		if fb, err := parseFirmwareBuild(b); err == nil {
			_ = fb
		}
		parseCoherencyUUID(b)
	})
}

// A drive that answers every command with arbitrary data or errors must
// never make Gather or the eject/firmware helpers panic or hang.
func FuzzGatherRandomDrive(f *testing.F) {
	f.Add([]byte{})
	f.Add(append([]byte{0, 10, 0x01, 0, 0, 0, 0, 0, 0, 0}, bytes.Repeat([]byte{0}, 40)...))
	f.Add(bytes.Repeat([]byte{0, 255}, 64))
	f.Add(bytes.Repeat([]byte{1, 1, 2, 0x3A, 0}, 64))
	f.Fuzz(func(t *testing.T, b []byte) {
		d := &randomDevice{data: b}
		info, err := Gather(d)
		if err == nil {
			info.Warnings()
			info.EncryptionAlgorithm()
			for _, e := range info.ErrorLog {
				e.Description()
				e.When()
				e.Operation()
			}
			AnalyzeErrorLog(info.ErrorLog, "x", info.Inquiry.Revision)
			if info.VHF != nil {
				info.VHF.ActivityName()
			}
		}
		d = &randomDevice{data: b}
		WriteProtected(d)
		TestUnitReady(d)
		ReadMicrocodeBuffer(d)
		q, _ := ReadInquiry(d)
		ReadFirmwareBuild(d, Inquiry{Vendor: "IBM"})
		CheckIBMImage(d, q, b)
	})
}

// randomDevice answers every command from the fuzz input: a control byte
// (bit 0: fail with the sense data that follows), a length byte (times 4),
// then that much data.
type randomDevice struct{ data []byte }

func (r *randomDevice) next(n int) []byte {
	n = min(n, len(r.data))
	b := r.data[:n]
	r.data = r.data[n:]
	return b
}

func (r *randomDevice) Do(cdb []byte, dir Direction, buf []byte, _ time.Duration) (int, error) {
	ctl := r.next(2)
	if len(ctl) < 2 {
		return 0, nil
	}
	data := r.next(int(ctl[1]) * 4)
	if ctl[0]&1 != 0 {
		return 0, newCommandError(cdb[0], ctl[0]>>1, data)
	}
	if dir != DirIn {
		return 0, nil
	}
	return copy(buf, data), nil
}

func (r *randomDevice) Path() string { return "/dev/sg-fuzz" }
func (r *randomDevice) Close() error { return nil }

func FuzzIBMImage(f *testing.F) {
	img := make([]byte, 0x100)
	binary.BigEndian.PutUint32(img[4:], uint32(len(img)))
	copy(img[0x20:], "IBMTpDrv")
	copy(img[0x40:], "2017/09/08")
	f.Add(img, append([]byte{0x01, 0x03, 0, 0x1c}, img[4:0x20]...))
	f.Fuzz(func(t *testing.T, b, vpd []byte) {
		im, err := ParseIBMImage(b)
		if err == nil {
			if int(binary.BigEndian.Uint32(b[4:])) != len(b) {
				t.Fatalf("accepted an image whose header length does not match")
			}
			if len(im.LoadID) != 4 || len(im.modelID) != 8 {
				t.Fatalf("bad IDs %+v", im)
			}
		}
		d := &randomDevice{data: append([]byte{0, byte((len(vpd) + 3) / 4)}, vpd...)}
		got, err := CheckIBMImage(d, Inquiry{Vendor: "IBM"}, b)
		if err == nil && got == nil {
			t.Fatalf("IBM drive passed without an image check")
		}
		if err == nil && (len(vpd) < 0x20 || !bytes.Equal(vpd[8:12], b[8:12]) || !bytes.Equal(vpd[0x18:0x20], b[0x18:0x20])) {
			t.Fatalf("image accepted for a drive it does not name")
		}
	})
}

func FuzzCheckFirmwareImage(f *testing.F) {
	f.Add(byte(0), 9462288, 0)
	f.Add(byte(20), 1, 256<<10)
	f.Add(byte(0x86), 9462288, 0)
	f.Add(byte(0xFF), 9462288, 0)
	f.Fuzz(func(t *testing.T, boundary byte, size, chunk int) {
		if size < 0 || size > 1<<25 {
			return
		}
		mb := parseMicrocodeBuffer([]byte{boundary, 0, 0, 0})
		chunkOut, err := CheckFirmwareImage(mb, make([]byte, size), chunk)
		if err != nil {
			return
		}
		align := DefaultFirmwareChunk
		if mb.Boundary != BoundaryUndefined {
			align = 1 << mb.Boundary
		}
		if chunkOut <= 0 || chunkOut%align != 0 || chunkOut >= maxOffset {
			t.Fatalf("chunk %d for boundary %d", chunkOut, boundary)
		}
		if size >= maxOffset || size == 0 {
			t.Fatalf("accepted %d bytes", size)
		}
		// Every piece must fit the 3-byte offset and length fields.
		for off := 0; off < size; off += chunkOut {
			if off >= maxOffset || min(chunkOut, size-off) >= maxOffset {
				t.Fatalf("piece at %d does not fit", off)
			}
		}
	})
}

func FuzzParseMounts(f *testing.F) {
	f.Add([]byte("ltfs:/dev/sg1 /mnt/ltfs\\040tape fuse rw 0 0\n"))
	f.Add([]byte("a \\ b\n\\7777 \\0 fuse.ltfs\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		ms, err := parseMounts(bytes.NewReader(b))
		if err != nil {
			return
		}
		for _, m := range ms {
			if len(m.source) > len(b) || len(m.target) > len(b) {
				t.Fatalf("unescaped field longer than the input")
			}
			if dev := m.ltfsDevice(); dev != "" && !strings.HasPrefix(m.source, "ltfs:") {
				t.Fatalf("LTFS device %q from source %q", dev, m.source)
			}
		}
	})
}
