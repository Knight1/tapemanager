package drive

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Locations read for discovery, replaceable in tests.
var (
	sysRoot    = "/sys"
	devRoot    = "/dev"
	procMounts = "/proc/mounts"
)

// Found is a tape drive known to the kernel.
type Found struct {
	SG     string   // SCSI generic node, for example /dev/sg2
	Names  []string // tape node names of the same drive: st0, nst0, ...
	Vendor string
	Model  string
	Rev    string
}

// List returns the tape drives attached to the system, sorted by sg node.
func List() ([]Found, error) {
	dir := filepath.Join(sysRoot, "class", "scsi_generic")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Found
	for _, e := range ents {
		dev := filepath.Join(dir, e.Name(), "device")
		if readSys(filepath.Join(dev, "type")) != "1" {
			continue
		}
		f := Found{
			SG:     filepath.Join(devRoot, e.Name()),
			Vendor: readSys(filepath.Join(dev, "vendor")),
			Model:  readSys(filepath.Join(dev, "model")),
			Rev:    readSys(filepath.Join(dev, "rev")),
		}
		if names, err := os.ReadDir(filepath.Join(dev, "scsi_tape")); err == nil {
			for _, n := range names {
				f.Names = append(f.Names, n.Name())
			}
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b Found) int { return sgNumber(a.SG) - sgNumber(b.SG) })
	return out, nil
}

func sgNumber(path string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "sg"))
	return n
}

func readSys(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 4096 {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// mount is one line of /proc/mounts.
type mount struct {
	source, target, fstype string
}

func readMounts() ([]mount, error) {
	f, err := os.Open(procMounts)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMounts(f)
}

func parseMounts(r io.Reader) ([]mount, error) {
	var out []mount
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		out = append(out, mount{unescapeMount(fields[0]), unescapeMount(fields[1]), fields[2]})
	}
	return out, sc.Err()
}

// unescapeMount decodes the octal escapes (\040 for a space) of /proc/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				sb.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// ltfsDevice returns the device an LTFS mount uses, as LTFS names it in the
// mount source ("ltfs:/dev/st0"), or "" for other filesystems.
func (m mount) ltfsDevice() string {
	dev, ok := strings.CutPrefix(m.source, "ltfs:")
	if !ok || m.fstype != "fuse" && !strings.HasPrefix(m.fstype, "fuse.") {
		return ""
	}
	return dev
}

// uses reports whether an LTFS device name refers to the drive f. LTFS
// accepts a device node, a /dev/tape/by-id link or the drive serial.
func (f Found) uses(dev, serial string) bool {
	if dev == "" {
		return false
	}
	if serial != "" && dev == serial {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(dev); err == nil {
		dev = resolved
	}
	base := filepath.Base(dev)
	return base == filepath.Base(f.SG) || slices.Contains(f.Names, base)
}

// MountPoints returns where tapes in drive f are mounted with LTFS. serial is
// the drive serial if known, since LTFS also accepts it as device name.
func MountPoints(f Found, serial string) ([]string, error) {
	mounts, err := readMounts()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range mounts {
		if f.uses(m.ltfsDevice(), serial) {
			out = append(out, m.target)
		}
	}
	return out, nil
}

// LTFSMounts returns the mount points of all LTFS mounts.
func LTFSMounts() ([]string, error) {
	mounts, err := readMounts()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range mounts {
		if m.ltfsDevice() != "" {
			out = append(out, m.target)
		}
	}
	return out, nil
}

// ForMount returns the drive whose tape is mounted at dir.
func ForMount(dir string) (Found, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Found{}, err
	}
	resolved := abs
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		resolved = r
	}
	mounts, err := readMounts()
	if err != nil {
		return Found{}, err
	}
	drives, err := List()
	if err != nil {
		return Found{}, err
	}
	for _, m := range slices.Backward(mounts) { // the last mount on a path wins
		if m.target != abs && m.target != resolved {
			continue
		}
		dev := m.ltfsDevice()
		if dev == "" {
			return Found{}, fmt.Errorf("%s is not an LTFS mount", dir)
		}
		for _, f := range drives {
			if f.uses(dev, "") {
				return f, nil
			}
		}
		return Found{}, fmt.Errorf("no tape drive found for %s (mounted from %s)", dir, dev)
	}
	return Found{}, fmt.Errorf("%s is not a mount point", dir)
}

// Resolve picks the drive to use: the given sg path, else the drive of the
// tape mounted at tapeRoot, else the only drive attached.
func Resolve(path, tapeRoot string) (Found, error) {
	drives, err := List()
	if err != nil {
		return Found{}, err
	}
	if path != "" {
		if r, err := filepath.EvalSymlinks(path); err == nil {
			path = r
		}
		for _, f := range drives {
			if f.SG == path || f.uses(path, "") {
				return f, nil
			}
		}
		return Found{SG: path}, nil
	}
	if tapeRoot != "" {
		if f, err := ForMount(tapeRoot); err == nil {
			return f, nil
		}
	}
	switch len(drives) {
	case 0:
		return Found{}, errors.New("no tape drive found")
	case 1:
		return drives[0], nil
	}
	names := make([]string, len(drives))
	for i, f := range drives {
		names[i] = f.SG
	}
	return Found{}, fmt.Errorf("several tape drives found (%s); choose one with --device", strings.Join(names, ", "))
}
