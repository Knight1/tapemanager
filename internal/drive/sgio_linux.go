package drive

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// Linux SCSI generic interface, see <scsi/sg.h>.
const (
	sgIO            = 0x2285
	sgGetVersionNum = 0x2282

	sgDxferNone    = -1
	sgDxferToDev   = -2
	sgDxferFromDev = -3

	sgInfoOKMask = 0x1
	driverSense  = 0x08
)

// sgIOHdr mirrors struct sg_io_hdr. Go aligns the pointer fields the same
// way the C compiler does, so no explicit padding is needed.
type sgIOHdr struct {
	interfaceID    int32
	dxferDirection int32
	cmdLen         uint8
	mxSbLen        uint8
	iovecCount     uint16
	dxferLen       uint32
	dxferp         unsafe.Pointer
	cmdp           unsafe.Pointer
	sbp            unsafe.Pointer
	timeout        uint32
	flags          uint32
	packID         int32
	usrPtr         unsafe.Pointer
	status         uint8
	maskedStatus   uint8
	msgStatus      uint8
	sbLenWr        uint8
	hostStatus     uint16
	driverStatus   uint16
	resid          int32
	duration       uint32
	info           uint32
}

// sgDevice sends commands through a /dev/sgN node.
type sgDevice struct {
	f    *os.File
	path string
}

// Open opens a SCSI generic device such as /dev/sg2. Only sg nodes are
// accepted: the st nodes are held by LTFS while a tape is mounted, and the
// sg node can still be queried then.
func Open(path string) (Device, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Mode()&os.ModeCharDevice == 0 {
		f.Close()
		return nil, fmt.Errorf("%s is not a character device", path)
	}
	d := &sgDevice{f: f, path: path}
	var ver int32
	if err := d.ioctl(sgGetVersionNum, unsafe.Pointer(&ver)); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s is not a SCSI generic device (use /dev/sgN, see 'tapemgr drive list')", path)
	}
	return d, nil
}

func (d *sgDevice) Close() error { return d.f.Close() }

func (d *sgDevice) Path() string { return d.path }

func (d *sgDevice) ioctl(req uintptr, arg unsafe.Pointer) error {
	rc, err := d.f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// Do sends one command. buf is filled for DirIn and sent for DirOut. It
// returns the number of bytes actually transferred.
func (d *sgDevice) Do(cdb []byte, dir Direction, buf []byte, timeout time.Duration) (int, error) {
	if len(cdb) == 0 || len(cdb) > 16 {
		return 0, errors.New("invalid command length")
	}
	if len(buf) > 1<<24 {
		return 0, errors.New("transfer too large")
	}
	sense := make([]byte, 64)
	cmd := append([]byte(nil), cdb...)
	h := sgIOHdr{
		interfaceID:    'S',
		dxferDirection: sgDxferNone,
		cmdLen:         uint8(len(cmd)),
		mxSbLen:        uint8(len(sense)),
		cmdp:           unsafe.Pointer(&cmd[0]),
		sbp:            unsafe.Pointer(&sense[0]),
		timeout:        uint32(min(timeout.Milliseconds(), 1<<31)),
	}
	if len(buf) > 0 {
		switch dir {
		case DirIn:
			h.dxferDirection = sgDxferFromDev
		case DirOut:
			h.dxferDirection = sgDxferToDev
		default:
			return 0, errors.New("buffer given without a direction")
		}
		h.dxferLen = uint32(len(buf))
		h.dxferp = unsafe.Pointer(&buf[0])
	}
	err := d.ioctl(sgIO, unsafe.Pointer(&h))
	runtime.KeepAlive(cmd)
	runtime.KeepAlive(sense)
	runtime.KeepAlive(buf)
	if err != nil {
		return 0, fmt.Errorf("SG_IO: %w", err)
	}
	n := len(buf) - int(h.resid)
	if n < 0 || n > len(buf) {
		n = 0
	}
	if h.info&sgInfoOKMask == 0 {
		return n, nil
	}
	if h.status != 0 || h.driverStatus&driverSense != 0 {
		return n, newCommandError(cdb[0], h.status, sense[:min(int(h.sbLenWr), len(sense))])
	}
	return n, fmt.Errorf("command 0x%02x failed: host status 0x%x, driver status 0x%x", cdb[0], h.hostStatus, h.driverStatus)
}
