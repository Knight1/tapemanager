package ltfs

import (
	"fmt"
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
