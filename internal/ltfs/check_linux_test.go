package ltfs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
	for line := range strings.SplitSeq(string(b), "\n") {
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

// On an LTFS mount every failure to force the index counts, "not
// supported" included; elsewhere there is no index to force.
func TestSyncIndexErrors(t *testing.T) {
	defer func() { setxattr, volumeUUID = syscall.Setxattr, VolumeUUID }()
	for _, errno := range []error{syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENODATA, syscall.EACCES, syscall.EIO} {
		setxattr = func(string, string, []byte, int) error { return errno }
		volumeUUID = func(string) string { return "00000000-0000-4000-8000-000000000001" }
		if err := SyncIndex("/mnt/ltfs"); !errors.Is(err, errno) {
			t.Errorf("LTFS, %v: err = %v", errno, err)
		}
		volumeUUID = func(string) string { return "" }
		if err := SyncIndex("/tmp/x"); err != nil {
			t.Errorf("not LTFS, %v: err = %v", errno, err)
		}
	}
	setxattr = func(string, string, []byte, int) error { return nil }
	volumeUUID = func(string) string { return "00000000-0000-4000-8000-000000000001" }
	if err := SyncIndex("/mnt/ltfs"); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeUUIDPattern(t *testing.T) {
	for in, ok := range map[string]bool{
		"00000000-0000-4000-8000-000000000001": true,
		"00000000-0000-4000-8000-00000000000G": false,
		"../../etc":                            false,
		"":                                     false,
	} {
		if uuidPattern.MatchString(in) != ok {
			t.Errorf("%q", in)
		}
	}
}
