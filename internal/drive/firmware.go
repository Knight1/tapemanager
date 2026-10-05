package drive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	opWriteBuffer = 0x3B
	opReadBuffer  = 0x3C
	// READ BUFFER mode: descriptor of a buffer (offset boundary, capacity).
	rbModeDescriptor = 0x03
	// WRITE BUFFER mode: download microcode with offsets, save and
	// activate. The drive activates the image only once all of it arrived,
	// so an interrupted download leaves the old firmware in place.
	wbModeMicrocodeOffsetsSave = 0x07
	microcodeBufferID          = 0x00
	// maxOffset is the limit of the 3-byte buffer offset field.
	maxOffset = 1 << 24
	// DefaultFirmwareChunk is small enough for every SCSI host adapter.
	DefaultFirmwareChunk = 256 << 10
)

// MicrocodeBuffer describes where firmware images are downloaded to.
type MicrocodeBuffer struct {
	Boundary int // offsets must be multiples of 1 << Boundary
	Capacity int // largest image the drive accepts, in bytes
}

// ReadMicrocodeBuffer asks the drive about its microcode buffer.
func ReadMicrocodeBuffer(d Device) (MicrocodeBuffer, error) {
	buf := make([]byte, 4)
	n, err := d.Do([]byte{opReadBuffer, rbModeDescriptor, microcodeBufferID, 0, 0, 0, 0, 0, 4, 0}, DirIn, buf, shortTimeout)
	if err != nil {
		return MicrocodeBuffer{}, err
	}
	if n < 4 {
		return MicrocodeBuffer{}, errShort
	}
	return MicrocodeBuffer{Boundary: int(buf[0]), Capacity: int(buf[1])<<16 | int(buf[2])<<8 | int(buf[3])}, nil
}

// FirmwareError is a download that failed partway. The drive activates an
// image only once it is complete, so it keeps running its old firmware.
type FirmwareError struct {
	Offset int
	Err    error
}

func (e *FirmwareError) Error() string {
	return fmt.Sprintf("firmware download failed at byte %d: %v", e.Offset, e.Err)
}

func (e *FirmwareError) Unwrap() error { return e.Err }

// CheckFirmwareImage checks that image can be downloaded to a drive with
// buffer b, and returns the piece size to use (at most chunk). The buffer
// capacity the drive reports is not a limit for microcode: IBM LTO-6 images
// are larger than the 5 MiB it reports for buffer 0 and are accepted. The
// only limit is the 3-byte offset field of WRITE BUFFER.
func CheckFirmwareImage(b MicrocodeBuffer, image []byte, chunk int) (int, error) {
	switch {
	case len(image) == 0:
		return 0, errors.New("the firmware file is empty")
	case len(image) >= maxOffset:
		return 0, fmt.Errorf("the firmware file (%d bytes) is larger than WRITE BUFFER can address (%d bytes)", len(image), maxOffset-1)
	case b.Boundary > 20:
		return 0, fmt.Errorf("the drive requires offsets aligned to 2^%d bytes, which is not supported", b.Boundary)
	}
	align := 1 << b.Boundary
	if chunk <= 0 {
		chunk = DefaultFirmwareChunk
	}
	chunk = chunk / align * align
	if chunk == 0 {
		chunk = align
	}
	return chunk, nil
}

// IBMImage is what the header of an IBM tape drive firmware image says.
type IBMImage struct {
	Level   string // firmware level, as INQUIRY will report it
	LoadID  []byte // drive type the image is for
	ModelID string // EBCDIC model identifier, decoded
	modelID []byte
	Built   string // build date if the header names one (YYYY/MM/DD)
}

// ErrNotIBMImage means the file has no IBM tape drive firmware header.
var ErrNotIBMImage = errors.New("not an IBM tape drive firmware image")

var buildDate = regexp.MustCompile(`20[0-9]{2}/[01][0-9]/[0-3][0-9]`)

// ParseIBMImage reads the header of an IBM tape drive firmware image: the
// length at byte 4, the load ID at 8, the level at 12, an EBCDIC model ID
// at 0x18 and the magic "IBMTpDrv" at 0x20. Bytes 8 to 0x20 have the same
// layout as the drive's VPD page 0x03.
func ParseIBMImage(b []byte) (*IBMImage, error) {
	if len(b) < 0x28 || string(b[0x20:0x28]) != "IBMTpDrv" {
		return nil, ErrNotIBMImage
	}
	if n := binary.BigEndian.Uint32(b[4:]); int64(n) != int64(len(b)) {
		return nil, fmt.Errorf("the image header says %d bytes but the file has %d: truncated or damaged", n, len(b))
	}
	img := &IBMImage{
		Level:   printable(b[12:16]),
		LoadID:  bytes.Clone(b[8:12]),
		modelID: bytes.Clone(b[0x18:0x20]),
	}
	img.ModelID = ebcdic(img.modelID)
	if m := buildDate.Find(b[:min(len(b), 4096)]); m != nil {
		img.Built = string(m)
	}
	return img, nil
}

// ebcdic decodes the letters, digits and spaces of an EBCDIC string.
func ebcdic(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c >= 0xC1 && c <= 0xC9:
			sb.WriteByte('A' + c - 0xC1)
		case c >= 0xD1 && c <= 0xD9:
			sb.WriteByte('J' + c - 0xD1)
		case c >= 0xE2 && c <= 0xE9:
			sb.WriteByte('S' + c - 0xE2)
		case c >= 0xF0 && c <= 0xF9:
			sb.WriteByte('0' + c - 0xF0)
		case c == 0x40:
			sb.WriteByte(' ')
		case c == 0:
		default:
			sb.WriteByte('?')
		}
	}
	return strings.TrimSpace(sb.String())
}

// CheckIBMImage compares an IBM image with the drive it is meant for, using
// the drive's VPD page 0x03 (load ID and model ID). It returns nil, nil for
// drives of other vendors, which have no such check here.
func CheckIBMImage(d Device, q Inquiry, image []byte) (*IBMImage, error) {
	if q.Vendor != "IBM" {
		return nil, nil
	}
	img, err := ParseIBMImage(image)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 255)
	n, err := d.Do(inquiryCDB(true, 0x03, uint16(len(buf))), DirIn, buf, shortTimeout)
	if err != nil {
		return nil, fmt.Errorf("reading the drive's load ID (VPD 0x03): %w", err)
	}
	v := buf[:n]
	if len(v) < 0x20 || v[1] != 0x03 {
		return nil, errors.New("the drive's load ID page (VPD 0x03) is too short")
	}
	if !bytes.Equal(v[8:12], img.LoadID) || !bytes.Equal(v[0x18:0x20], img.modelID) {
		return nil, fmt.Errorf("the image is for another drive model (image %s / %x, drive %s / %x)",
			img.ModelID, img.LoadID, ebcdic(v[0x18:0x20]), v[8:12])
	}
	return img, nil
}

// UpdateFirmware downloads image to the drive in pieces of chunk bytes and
// lets the drive save and activate it. The drive resets afterwards, which
// can take minutes; use WaitForDrive to see it come back. progress, if not
// nil, is called after every piece.
func UpdateFirmware(d Device, image []byte, chunk int, progress func(done, total int)) error {
	b, err := ReadMicrocodeBuffer(d)
	if err != nil {
		return fmt.Errorf("reading the microcode buffer: %w", err)
	}
	if chunk, err = CheckFirmwareImage(b, image, chunk); err != nil {
		return err
	}
	for off := 0; off < len(image); off += chunk {
		piece := image[off:min(off+chunk, len(image))]
		cdb := []byte{opWriteBuffer, wbModeMicrocodeOffsetsSave, microcodeBufferID,
			byte(off >> 16), byte(off >> 8), byte(off),
			byte(len(piece) >> 16), byte(len(piece) >> 8), byte(len(piece)), 0}
		// The last piece includes saving to flash, which takes a while.
		if _, err := d.Do(cdb, DirOut, piece, moveTimeout); err != nil {
			return &FirmwareError{Offset: off, Err: err}
		}
		if progress != nil {
			progress(off+len(piece), len(image))
		}
	}
	return nil
}

// WaitForDrive polls until the drive answers INQUIRY again after a
// firmware update, and returns what it reports. open is called for every
// attempt, because the device can disappear while the drive resets.
func WaitForDrive(open func() (Device, error), timeout, interval time.Duration) (Inquiry, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if d, err := open(); err == nil {
			q, err := ReadInquiry(d)
			if err == nil {
				// A pending unit attention is reported once after the reset.
				TestUnitReady(d)
			}
			d.Close()
			if err == nil {
				return q, nil
			}
			last = err
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return Inquiry{}, fmt.Errorf("the drive did not come back within %s: %v", timeout, last)
		}
		time.Sleep(interval)
	}
}

// FirmwareBuild describes the installed firmware beyond its level, from
// IBM's firmware designation page (VPD 0xC0). It is the same for every
// drive running that firmware.
type FirmwareBuild struct {
	Name     string // for example LTO6_E6R3
	Built    string // YYYY-MM-DD HH:MM:SS
	Platform string // for example sas_hh (SAS, half height)
}

// ReadFirmwareBuild reads the firmware designation of IBM drives. Other
// vendors use the page differently, so they get ErrUnsupported.
func ReadFirmwareBuild(d Device, q Inquiry) (FirmwareBuild, error) {
	if q.Vendor != "IBM" {
		return FirmwareBuild{}, ErrUnsupported
	}
	buf := make([]byte, 255)
	n, err := d.Do(inquiryCDB(true, 0xC0, uint16(len(buf))), DirIn, buf, shortTimeout)
	if err != nil {
		return FirmwareBuild{}, err
	}
	return parseFirmwareBuild(buf[:n])
}

func parseFirmwareBuild(b []byte) (FirmwareBuild, error) {
	if len(b) < 43 || b[1] != 0xC0 {
		return FirmwareBuild{}, errShort
	}
	digits := func(s []byte) bool {
		for _, c := range s {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	fb := FirmwareBuild{Name: printable(b[4:16]), Platform: printable(b[31:43])}
	t, d := b[16:22], b[23:31]
	if digits(t) && digits(d) {
		fb.Built = fmt.Sprintf("%s-%s-%s %s:%s:%s", d[0:4], d[4:6], d[6:8], t[0:2], t[2:4], t[4:6])
	}
	return fb, nil
}
