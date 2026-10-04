package ltfs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPlainDirectoryIsNotLTFS(t *testing.T) {
	dir := t.TempDir()
	if err := CheckMounted(dir); err == nil {
		t.Error("plain directory accepted as LTFS mount")
	}
	if id := VolumeUUID(dir); id != "" {
		t.Errorf("VolumeUUID = %q", id)
	}
	if err := CheckMounted(dir + "/missing"); err == nil {
		t.Error("missing path accepted")
	}
}

func TestStartBlockUnavailable(t *testing.T) {
	if _, ok := StartBlock(t.TempDir()); ok {
		t.Error("start block reported for a plain directory")
	}
}

func TestFreeSpace(t *testing.T) {
	n, err := FreeSpace(t.TempDir())
	if err != nil || n <= 0 {
		t.Fatalf("%d, %v", n, err)
	}
	if _, err := FreeSpace("/does/not/exist"); err == nil {
		t.Error("missing path accepted")
	}
}

func TestSyncIndexIgnoresPlainDirectories(t *testing.T) {
	if err := SyncIndex(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnly(t *testing.T) {
	if ro, err := ReadOnly(t.TempDir()); err != nil || ro {
		t.Fatalf("temp dir: %v %v", ro, err)
	}
	if _, err := ReadOnly(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing path accepted")
	}
	// Find a read-only mount on this host, if any.
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		t.Skip(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !slices.Contains(strings.Split(f[3], ","), "ro") || strings.Contains(f[1], `\`) {
			continue
		}
		if ro, err := ReadOnly(f[1]); err == nil {
			if !ro {
				t.Fatalf("%s is mounted ro but not reported", f[1])
			}
			return
		}
	}
	t.Log("no read-only mount found to check")
}
