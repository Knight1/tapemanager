package ltfs

import (
	"fmt"
	"strconv"
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
	return string(buf[:n])
}

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

// FreeSpace returns the bytes available on the filesystem holding path.
// LTFS reports the remaining capacity of the mounted tape.
func FreeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
