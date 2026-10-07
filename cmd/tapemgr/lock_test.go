package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Knight1/tapemanager/internal/flock"
)

// While a command that writes or moves the drive runs, nothing else that
// touches a tape or the drive may start; readers may run together.
func TestGlobalLock(t *testing.T) {
	l, err := flock.Path(lockPath, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"archive", "put", "/x"},
		{"archive", "verify"},
		{"drive", "eject"},
		{"drive", "info"},
		{"drive", "firmware", "--file", "x"},
	} {
		code, _, errOut := runCmd(t, args...)
		if code != exitFailure || !strings.Contains(errOut, "'"+args[0]+" "+args[1]+"'") || !strings.Contains(errOut, "has finished") {
			t.Errorf("%v while exclusive: %d %s", args, code, errOut)
		}
	}
	// Purely local commands need no lock.
	if code, out, _ := runCmd(t, "drive", "keygen", "--out", t.TempDir()+"/k"); code != 0 || !strings.Contains(out, "Added key") {
		t.Errorf("keygen while locked: %d %s", code, out)
	}
	l.Unlock()

	s, err := flock.Path(lockPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, errOut := runCmd(t, "drive", "info"); strings.Contains(errOut, "has finished") {
		t.Errorf("reader next to a reader: %s", errOut)
	}
	if code, _, errOut := runCmd(t, "drive", "load"); code != exitFailure || !strings.Contains(errOut, "must run alone") {
		t.Errorf("load next to a reader: %d %s", code, errOut)
	}
	s.Unlock()
}

// Every command is either in commandLocks or known to need no lock, so a
// new command cannot silently run unlocked.
func TestEveryCommandDecidesItsLock(t *testing.T) {
	free := map[string]bool{"archive keygen": true, "drive keygen": true, "drive inspect-firmware": true, "drive list": true, "catalog tapes": true, "catalog search": true}
	for _, m := range regexp.MustCompile(`(?m)^  tapemgr (\w+ [\w-]+)`).FindAllStringSubmatch(usage, -1) {
		cmd := m[1]
		if _, ok := commandLocks[cmd]; !ok && !free[cmd] {
			t.Errorf("%q has no lock decision", cmd)
		}
	}
}
