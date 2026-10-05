package ltfs

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// fuseSuperMagic is the statfs f_type reported for FUSE mounts, which is how
// LTFS is mounted on Linux.
const fuseSuperMagic = 0x65735546

// CheckMounted returns an error unless path is on a FUSE filesystem.
// This guards against writing to an empty mount point directory and filling
// the local disk when the tape is not actually mounted.
func CheckMounted(path string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", path, err)
	}
	if st.Type != fuseSuperMagic {
		return fmt.Errorf("%s is not an LTFS (FUSE) mount; mount the tape first or pass --no-mount-check", path)
	}
	return nil
}

// VolumeUUID returns the LTFS volume UUID of the tape mounted at path, or ""
// if path is not an LTFS mount.
func VolumeUUID(path string) string {
	buf := make([]byte, 128)
	n, err := syscall.Getxattr(path, "user.ltfs.volumeUUID", buf)
	if err != nil {
		return ""
	}
	// The value comes from the tape and ends up in the catalog.
	u := strings.TrimRight(string(buf[:n]), "\x00\n")
	if !uuidPattern.MatchString(u) {
		return ""
	}
	return strings.ToLower(u)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// StartBlock returns the tape block where the file at path begins, as
// reported by LTFS, so files can be read in tape order. ok is false if
// the information is not available.
func StartBlock(path string) (block int64, ok bool) {
	buf := make([]byte, 32)
	n, err := syscall.Getxattr(path, "user.ltfs.startblock", buf)
	if err != nil {
		return 0, false
	}
	block, err = strconv.ParseInt(string(buf[:n]), 10, 64)
	return block, err == nil && block >= 0
}

// stRdonly is the statfs flag of a read-only mount.
const stRdonly = 0x1

// ReadOnly reports whether path is on a read-only mount. LTFS mounts a
// write-protected cartridge read-only.
func ReadOnly(path string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, err
	}
	return st.Flags&stRdonly != 0, nil
}

// FreeSpace returns the bytes available on the filesystem holding path.
// LTFS reports the remaining capacity of the mounted tape.
func FreeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// Replaceable in tests.
var (
	setxattr   = syscall.Setxattr
	volumeUUID = VolumeUUID
)

// SyncIndex makes LTFS write its index to tape now, so everything written
// so far survives a host crash. LTFS otherwise writes the index only every
// few minutes or at unmount, and after a crash rolls back to the last one.
// It returns nil for directories that are not LTFS mounts (no volume UUID).
// On LTFS every error counts, including "not supported": callers drop
// their local copy of records once this returns nil.
func SyncIndex(root string) error {
	err := setxattr(root, "user.ltfs.sync", []byte("1"), 0)
	if err == nil {
		return nil
	}
	if volumeUUID(root) == "" {
		return nil // not LTFS, for example a test directory
	}
	return fmt.Errorf("forcing the LTFS index to tape: %w", err)
}
