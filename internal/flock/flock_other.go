//go:build !unix

package flock

import (
	"errors"
	"os"
)

// ErrBusy means another process holds a conflicting lock.
var ErrBusy = errors.New("in use by another tapemgr process")

// Lock is a held lock. There is no locking on this platform.
type Lock struct{}

func File(*os.File, bool) (*Lock, error) { return &Lock{}, nil }
func Path(string, bool) (*Lock, error)   { return &Lock{}, nil }
func (l *Lock) Unlock() error            { return nil }
