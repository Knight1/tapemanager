package drive

import (
	"errors"
	"fmt"
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
// buffer b, and returns the piece size to use (at most chunk).
func CheckFirmwareImage(b MicrocodeBuffer, image []byte, chunk int) (int, error) {
	switch {
	case len(image) == 0:
		return 0, errors.New("the firmware file is empty")
	case b.Capacity <= 0:
		return 0, errors.New("the drive reports no microcode buffer")
	case len(image) > b.Capacity:
		return 0, fmt.Errorf("the firmware file (%d bytes) is larger than the drive's microcode buffer (%d bytes); is it for this drive?", len(image), b.Capacity)
	case len(image) >= maxOffset:
		return 0, errors.New("the firmware file is too large")
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
