package memcheck

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeSys(t *testing.T, meminfo string, cgroups map[string]map[string]string) {
	t.Helper()
	dir := t.TempDir()
	old := [3]string{procMeminfo, procCgroup, cgroupRoot}
	t.Cleanup(func() { procMeminfo, procCgroup, cgroupRoot = old[0], old[1], old[2] })
	procMeminfo = filepath.Join(dir, "meminfo")
	procCgroup = filepath.Join(dir, "cgroup")
	cgroupRoot = filepath.Join(dir, "cg")
	os.WriteFile(procMeminfo, []byte(meminfo), 0o644)
	os.WriteFile(procCgroup, []byte("0::/system.slice/run-x.scope\n"), 0o644)
	for rel, files := range cgroups {
		d := filepath.Join(cgroupRoot, rel)
		os.MkdirAll(d, 0o755)
		for name, v := range files {
			os.WriteFile(filepath.Join(d, name), []byte(v), 0o644)
		}
	}
}

const meminfo = "MemTotal: 8000000 kB\nMemAvailable: 4000000 kB\nSwapFree: 1000000 kB\n"

func TestHostOnly(t *testing.T) {
	fakeSys(t, meminfo, map[string]map[string]string{"system.slice/run-x.scope": {"memory.max": "max\n"}})
	n, err := Available()
	if err != nil || n != 5000000*1024 {
		t.Fatalf("%d %v", n, err)
	}
}

// A memory limit on the process's cgroup or an ancestor wins over the
// host's free memory; file cache counts as free; limited swap counts.
func TestCgroupLimit(t *testing.T) {
	fakeSys(t, meminfo, map[string]map[string]string{
		"system.slice/run-x.scope": {"memory.max": "1610612736\n", "memory.current": "1073741824\n",
			"memory.stat": "anon 900000000\ninactive_file 268435456\n", "memory.swap.max": "0\n", "memory.swap.current": "0\n"},
		"system.slice": {"memory.max": "max\n"},
	})
	n, err := Available()
	if want := int64(1610612736 - 1073741824 + 268435456); err != nil || n != want {
		t.Fatalf("%d %v, want %d", n, err, want)
	}
	if err := Ensure(700<<20, "repairing x"); err != nil {
		t.Fatalf("fits: %v", err)
	}
	if err := Ensure(800<<20, "repairing x"); !errors.Is(err, ErrNoMemory) || !strings.Contains(err.Error(), "repairing x needs about 800 MiB") {
		t.Fatalf("too much: %v", err)
	}
}

func TestAncestorLimit(t *testing.T) {
	fakeSys(t, meminfo, map[string]map[string]string{
		"system.slice/run-x.scope": {"memory.max": "max\n"},
		"system.slice":             {"memory.max": "2147483648\n", "memory.current": "2147483648\n"},
	})
	if n, _ := Available(); n != 1000000*1024 { // only host swap left
		t.Fatalf("%d", n)
	}
}

// Without readable information the check never blocks.
func TestUnknown(t *testing.T) {
	fakeSys(t, "garbage\n", nil)
	if _, err := Available(); err == nil {
		t.Fatal("garbage meminfo accepted")
	}
	if err := Ensure(1<<50, "x"); err != nil {
		t.Fatal(err)
	}
	procMeminfo = "/does/not/exist"
	if err := Ensure(1<<50, "x"); err != nil {
		t.Fatal(err)
	}
}

func TestRealHost(t *testing.T) {
	if n, err := Available(); err == nil && n <= 0 {
		t.Fatalf("available %d", n)
	}
}
