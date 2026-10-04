package ltfs

import "testing"

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
