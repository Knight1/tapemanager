//go:build unix

package flock

import (
	"errors"
	"os"
	"testing"
)

func TestExclusiveAndShared(t *testing.T) {
	dir := t.TempDir() + "/lock"
	a, err := Path(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Path(dir, false)
	if err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	if _, err := Path(dir, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("exclusive while shared: %v", err)
	}
	a.Unlock()
	b.Unlock()
	x, err := Path(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Path(dir, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("shared while exclusive: %v", err)
	}
	x.Unlock()
	if l, err := Path(dir, true); err != nil {
		t.Fatalf("after unlock: %v", err)
	} else {
		l.Unlock()
	}
	if _, err := Path(t.TempDir()+"/no/such/dir", true); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory: %v", err)
	}
	link := t.TempDir() + "/link"
	os.Symlink(t.TempDir()+"/target", link)
	if _, err := Path(link, true); err == nil {
		t.Fatal("symlink followed")
	}
	var nilLock *Lock
	if nilLock.Unlock() != nil {
		t.Fatal("nil unlock")
	}
}
