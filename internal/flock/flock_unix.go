//go:build unix

// Package flock takes advisory locks (flock), so that tapemgr commands
// that write to a tape or move the drive never run next to each other. The kernel drops
// a lock when its process ends, also on a crash or SIGKILL, so a stale lock
// can never block the next run.
package flock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrBusy means another process holds a conflicting lock.
var ErrBusy = errors.New("in use by another tapemgr process")

// Lock is a held lock.
type Lock struct{ f *os.File }

// File locks the open file f without waiting: exclusive for writers,
// shared for readers. Closing f, or Unlock, releases it.
func File(f *os.File, exclusive bool) (*Lock, error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) {
		for {
			ferr = syscall.Flock(int(fd), how|syscall.LOCK_NB)
			if ferr != syscall.EINTR {
				return
			}
		}
	}); err != nil {
		return nil, err
	}
	switch {
	case errors.Is(ferr, syscall.EWOULDBLOCK):
		return nil, ErrBusy
	case ferr != nil:
		return nil, fmt.Errorf("lock %s: %w", f.Name(), ferr)
	}
	return &Lock{f: f}, nil
}

// Path opens path, creating it as an empty file if needed, and locks it;
// Unlock closes it. A symlink at path is refused: lock files live in
// directories every user can write to, such as /run/lock.
func Path(path string, exclusive bool) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, err
	}
	l, err := File(f, exclusive)
	if err != nil {
		f.Close()
		if errors.Is(err, ErrBusy) {
			return nil, fmt.Errorf("%s is %w", path, ErrBusy)
		}
		return nil, err
	}
	return l, nil
}

// Unlock releases the lock and closes the file it was taken on.
func (l *Lock) Unlock() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}
