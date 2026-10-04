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
