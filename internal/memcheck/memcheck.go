// Package memcheck checks before a large allocation that the memory is
// there, so tapemgr fails with a clear message instead of being killed by
// the kernel's out-of-memory killer. It is a snapshot: other processes can
// take memory right after the check, so it prevents most such kills, not
// all.
package memcheck

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Files read, replaceable in tests.
var (
	procMeminfo = "/proc/meminfo"
	procCgroup  = "/proc/self/cgroup"
	cgroupRoot  = "/sys/fs/cgroup"
)

// Margin is kept free on top of what an operation needs.
const Margin = 64 << 20

// ErrNoMemory is returned by Ensure when there is not enough memory.
var ErrNoMemory = errors.New("not enough free memory")

// errUnknown means the available memory cannot be determined.
var errUnknown = errors.New("available memory unknown")

// Available returns how many bytes this process can still allocate: the
// host's available memory plus free swap, limited by every cgroup v2
// memory limit above the process (systemd scopes, containers). Page cache
// counts as available, since the kernel drops it under pressure.
func Available() (int64, error) {
	mi, err := readKV(procMeminfo)
	if err != nil {
		return 0, err
	}
	avail, ok1 := mi["MemAvailable"]
	swap, ok2 := mi["SwapFree"]
	if !ok1 || !ok2 {
		return 0, errUnknown
	}
	best := (avail + swap) * 1024 // kB
	for _, dir := range cgroupDirs() {
		if h, ok := cgroupHeadroom(dir, swap*1024); ok && h < best {
			best = h
		}
	}
	return max(best, 0), nil
}

// Ensure returns an error if need bytes (plus Margin) are not available
// for what. If the available memory cannot be determined, it does not
// stand in the way.
func Ensure(need int64, what string) error {
	avail, err := Available()
	if err != nil {
		return nil
	}
	if need+Margin > avail {
		return fmt.Errorf("%w: %s needs about %s, but only %s is free (RAM and swap, within this process's memory limit); free some memory and try again", ErrNoMemory, what, mib(need), mib(avail))
	}
	return nil
}

func mib(n int64) string { return fmt.Sprintf("%d MiB", n>>20) }

// cgroupDirs returns the process's cgroup v2 directory and its ancestors.
func cgroupDirs() []string {
	b, err := os.ReadFile(procCgroup)
	if err != nil {
		return nil
	}
	var rel string
	for line := range strings.SplitSeq(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			rel = p
		}
	}
	if rel == "" || strings.Contains(rel, "..") {
		return nil
	}
	var dirs []string
	for d := filepath.Join(cgroupRoot, rel); strings.HasPrefix(d, cgroupRoot); d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == cgroupRoot {
			break
		}
	}
	return dirs
}

// cgroupHeadroom returns what can still be allocated in cgroup dir: memory
// below memory.max (reclaimable file cache counts as free) plus swap below
// memory.swap.max. hostSwap is used where swap is not limited. ok is false
// if the cgroup sets no memory limit.
func cgroupHeadroom(dir string, hostSwap int64) (int64, bool) {
	limit, ok := readLimit(filepath.Join(dir, "memory.max"))
	if !ok {
		return 0, false
	}
	cur, ok := readLimit(filepath.Join(dir, "memory.current"))
	if !ok {
		return 0, false
	}
	if st, err := readKV(filepath.Join(dir, "memory.stat")); err == nil {
		cur -= st["inactive_file"]
	}
	h := limit - cur
	swap := hostSwap
	if smax, ok := readLimit(filepath.Join(dir, "memory.swap.max")); ok {
		scur, _ := readLimit(filepath.Join(dir, "memory.swap.current"))
		swap = min(swap, smax-scur)
	}
	return h + max(swap, 0), true
}

// readLimit reads a cgroup number; "max" and unreadable files give false.
func readLimit(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}

// readKV reads "key value" or "key: value kB" lines into numbers.
func readKV(path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(strings.Replace(sc.Text(), ":", " ", 1))
		if len(fields) < 2 {
			continue
		}
		if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			m[fields[0]] = n
		}
	}
	return m, sc.Err()
}
