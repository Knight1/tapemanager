package drivetest

import (
	"time"

	"github.com/Knight1/tapemanager/internal/drive"
)

// Random is a drive that answers every command from a byte stream, for
// fuzzing: each answer takes a control byte (bit 0 set: the command fails
// with the sense key and codes that follow), a length byte (times 4), and
// that much response data. An exhausted stream answers with empty data.
type Random struct {
	Data []byte
}

func (r *Random) next(n int) []byte {
	n = min(n, len(r.Data))
	b := r.Data[:n]
	r.Data = r.Data[n:]
	return b
}

// Do implements drive.Device.
func (r *Random) Do(cdb []byte, dir drive.Direction, buf []byte, _ time.Duration) (int, error) {
	ctl := r.next(2)
	if len(ctl) < 2 {
		return 0, nil
	}
	data := r.next(int(ctl[1]) * 4)
	if ctl[0]&1 != 0 {
		e := &drive.CommandError{Op: cdb[0], Status: 0x02}
		if len(data) >= 3 {
			e.Key, e.ASC, e.ASCQ = data[0]&0x0F, data[1], data[2]
		}
		return 0, e
	}
	if dir != drive.DirIn {
		return 0, nil
	}
	return copy(buf, data), nil
}

// Path implements drive.Device.
func (r *Random) Path() string { return "/dev/sg-fuzz" }

// Close implements drive.Device.
func (r *Random) Close() error { return nil }
