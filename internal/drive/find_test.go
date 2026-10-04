package drive

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSys builds a sysfs and /dev tree with the given sg devices. Tape
// drives get st and nst names numbered like the kernel does.
func fakeSys(t *testing.T, devices map[string]string, mounts string) string {
	t.Helper()
	base := t.TempDir()
	oldSys, oldDev, oldMounts := sysRoot, devRoot, procMounts
	sysRoot, devRoot, procMounts = filepath.Join(base, "sys"), filepath.Join(base, "dev"), filepath.Join(base, "mounts")
	t.Cleanup(func() { sysRoot, devRoot, procMounts = oldSys, oldDev, oldMounts })
	os.MkdirAll(devRoot, 0o755)
	tapeNo := 0
	for sg, typ := range devices {
		dev := filepath.Join(sysRoot, "class", "scsi_generic", sg, "device")
		os.MkdirAll(dev, 0o755)
		os.WriteFile(filepath.Join(dev, "type"), []byte(typ+"\n"), 0o644)
		os.WriteFile(filepath.Join(dev, "vendor"), []byte("IBM     \n"), 0o644)
		os.WriteFile(filepath.Join(dev, "model"), []byte("ULT3580-HH6     \n"), 0o644)
		os.WriteFile(filepath.Join(dev, "rev"), []byte("E6R3\n"), 0o644)
		os.WriteFile(filepath.Join(devRoot, sg), nil, 0o644)
		if typ == "1" {
			for _, n := range []string{"st", "nst"} {
				name := n + string(rune('0'+tapeNo))
				os.MkdirAll(filepath.Join(dev, "scsi_tape", name), 0o755)
				os.WriteFile(filepath.Join(devRoot, name), nil, 0o644)
			}
			tapeNo++
		}
	}
	os.WriteFile(procMounts, []byte(mounts), 0o644)
	return base
}

func TestList(t *testing.T) {
	fakeSys(t, map[string]string{"sg10": "1", "sg2": "1", "sg0": "0"}, "")
	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SG != filepath.Join(devRoot, "sg2") || got[1].SG != filepath.Join(devRoot, "sg10") {
		t.Fatalf("%+v", got)
	}
	if got[0].Vendor != "IBM" || got[0].Model != "ULT3580-HH6" || got[0].Rev != "E6R3" || len(got[0].Names) != 2 {
		t.Fatalf("%+v", got[0])
	}
}

func TestListWithoutSysfs(t *testing.T) {
	fakeSys(t, nil, "")
	if got, err := List(); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestParseMounts(t *testing.T) {
	in := "ltfs:/dev/st0 /mnt/my\\040tape fuse rw 0 0\nshort line\n/dev/sda1 / ext4 rw 0 0\n"
	m, err := parseMounts(strings.NewReader(in))
	if err != nil || len(m) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	if m[0].target != "/mnt/my tape" || m[0].ltfsDevice() != "/dev/st0" {
		t.Fatalf("%+v", m[0])
	}
	if m[1].ltfsDevice() != "" {
		t.Fatal("ext4 treated as LTFS")
	}
	if (mount{source: "ltfs:/dev/st0", fstype: "ext4"}).ltfsDevice() != "" {
		t.Fatal("non-FUSE mount treated as LTFS")
	}
	if (mount{source: "ltfs:/dev/st0", fstype: "fuse.ltfs"}).ltfsDevice() != "/dev/st0" {
		t.Fatal("fuse.ltfs not recognized")
	}
	if got := unescapeMount(`a\134b\9zz\04`); got != `a\b\9zz\04` {
		t.Fatalf("%q", got)
	}
}

func TestMountPointsAndForMount(t *testing.T) {
	base := fakeSys(t, map[string]string{"sg2": "1", "sg3": "1"}, "")
	mnt := filepath.Join(base, "mnt")
	other := filepath.Join(base, "other")
	os.MkdirAll(mnt, 0o755)
	os.MkdirAll(other, 0o755)
	drives, _ := List()
	sg2, sg3 := drives[0], drives[1]
	// Find which st name belongs to which drive.
	st := func(f Found) string {
		for _, n := range f.Names {
			if strings.HasPrefix(n, "st") {
				return n
			}
		}
		return ""
	}
	byID := filepath.Join(devRoot, "by-id-link")
	os.Symlink(filepath.Join(devRoot, st(sg3)), byID)
	os.WriteFile(procMounts, []byte(strings.Join([]string{
		"ltfs:" + filepath.Join(devRoot, st(sg2)) + " " + mnt + " fuse rw 0 0",
		"ltfs:" + byID + " " + other + " fuse rw 0 0",
		"ltfs:SERIAL123 /serial fuse rw 0 0",
		"/dev/sda1 /data ext4 rw 0 0",
	}, "\n")), 0o644)

	if mps, err := MountPoints(sg2, ""); err != nil || !slices.Equal(mps, []string{mnt}) {
		t.Fatalf("%v %v", mps, err)
	}
	if mps, _ := MountPoints(sg3, "SERIAL123"); !slices.Equal(mps, []string{other, "/serial"}) {
		t.Fatalf("by-id and serial: %v", mps)
	}
	if all, _ := LTFSMounts(); len(all) != 3 {
		t.Fatalf("%v", all)
	}

	if f, err := ForMount(mnt); err != nil || f.SG != sg2.SG {
		t.Fatalf("%+v %v", f, err)
	}
	if f, err := ForMount(other); err != nil || f.SG != sg3.SG {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := ForMount("/data"); err == nil || !strings.Contains(err.Error(), "not an LTFS mount") {
		t.Fatalf("%v", err)
	}
	if _, err := ForMount(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a mount point") {
		t.Fatalf("%v", err)
	}
	if _, err := ForMount("/serial"); err == nil || !strings.Contains(err.Error(), "no tape drive found") {
		t.Fatalf("%v", err)
	}

	procMounts = filepath.Join(base, "missing")
	if _, err := MountPoints(sg2, ""); err == nil {
		t.Fatal("missing mounts file not reported")
	}
	if _, err := LTFSMounts(); err == nil {
		t.Fatal("missing mounts file not reported")
	}
}

func TestResolve(t *testing.T) {
	base := fakeSys(t, map[string]string{"sg2": "1"}, "")
	sg2 := filepath.Join(devRoot, "sg2")

	if f, err := Resolve("", ""); err != nil || f.SG != sg2 {
		t.Fatalf("only drive: %+v %v", f, err)
	}
	// A tape node given as device is mapped to the drive's sg node.
	if f, err := Resolve(filepath.Join(devRoot, "nst0"), ""); err != nil || f.SG != sg2 {
		t.Fatalf("nst path: %+v %v", f, err)
	}
	// Unknown paths are passed through for Open to check.
	if f, err := Resolve("/dev/sg77", ""); err != nil || f.SG != "/dev/sg77" || len(f.Names) != 0 {
		t.Fatalf("unknown path: %+v %v", f, err)
	}

	fakeSys(t, map[string]string{"sg2": "1", "sg3": "1"}, "")
	if _, err := Resolve("", ""); err == nil || !strings.Contains(err.Error(), "--device") {
		t.Fatalf("several drives: %v", err)
	}
	mnt := filepath.Join(base, "mnt")
	os.MkdirAll(mnt, 0o755)
	drives, _ := List()
	os.WriteFile(procMounts, []byte("ltfs:"+filepath.Join(devRoot, drives[1].Names[0])+" "+mnt+" fuse rw 0 0\n"), 0o644)
	if f, err := Resolve("", mnt); err != nil || f.SG != drives[1].SG {
		t.Fatalf("by mount: %+v %v", f, err)
	}

	fakeSys(t, map[string]string{"sg0": "0"}, "")
	if _, err := Resolve("", ""); err == nil || !strings.Contains(err.Error(), "no tape drive") {
		t.Fatalf("no drives: %v", err)
	}
}
