package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Knight1/tapemanager/internal/flock"
)

// lockPath is the one lock all tapemgr commands on this host share. The
// kernel releases it when a process ends, also on a crash, so it never
// goes stale.
var lockPath = defaultLockPath()

func defaultLockPath() string {
	if st, err := os.Stat("/run/lock"); err == nil && st.IsDir() {
		return "/run/lock/tapemgr.lock"
	}
	return filepath.Join(os.TempDir(), "tapemgr.lock")
}

type lockMode int

const (
	lockNone lockMode = iota
	lockShared
	lockExclusive
)

// commandLocks says which commands take the global lock. Commands that
// write to a tape, move the drive, change its firmware or delete files run
// alone. Commands that only read a tape or query the drive may run
// together, but not next to one of those. Purely local commands (key
// generation, catalog listings, drive list) take no lock.
var commandLocks = map[string]lockMode{
	"archive put":           lockExclusive,
	"archive recover":       lockExclusive,
	"archive repair-volume": lockExclusive,
	"archive purge-source":  lockExclusive,
	"catalog import":        lockExclusive,
	"catalog retire":        lockExclusive,
	"drive firmware":        lockExclusive,
	"drive load":            lockExclusive,
	"drive eject":           lockExclusive,
	"archive verify":        lockShared,
	"archive restore":       lockShared,
	"archive list":          lockShared,
	"drive info":            lockShared,
	"drive log":             lockShared,
	"drive check":           lockShared,
}

// takeLock takes the global lock for command, or returns nil if it needs
// none.
func takeLock(command string) (*flock.Lock, error) {
	mode := commandLocks[command]
	if mode == lockNone {
		return nil, nil
	}
	l, err := flock.Path(lockPath, mode == lockExclusive)
	if errors.Is(err, flock.ErrBusy) {
		if mode == lockExclusive {
			return nil, fmt.Errorf("another tapemgr command is running; '%s' must run alone, start it again when that one has finished", command)
		}
		return nil, fmt.Errorf("a tapemgr command that writes to the tape or moves the drive is running; start '%s' again when it has finished", command)
	}
	if err != nil {
		return nil, fmt.Errorf("taking the tapemgr lock %s: %w", lockPath, err)
	}
	return l, nil
}
