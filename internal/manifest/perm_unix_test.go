//go:build unix

package manifest

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteFileAtomicPerm(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	dir := t.TempDir()
	name := filepath.Join(dir, "keys")
	// A leftover temporary file with wide permissions is not reused.
	os.WriteFile(name+".tmp", []byte("stale"), 0o666)
	if err := WriteFileAtomicPerm(name, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(name)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st.Mode(), err)
	}
	if b, _ := os.ReadFile(name); string(b) != "secret" {
		t.Fatalf("%q", b)
	}
	if _, err := os.Stat(name + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
	if err := WriteFileAtomicPerm(filepath.Join(dir, "missing", "keys"), nil, 0o600); err == nil {
		t.Fatal("write into a missing directory succeeded")
	}
}
