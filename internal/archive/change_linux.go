package archive

import (
	"fmt"
	"io/fs"
	"syscall"
)

// fileChangeID identifies one version of a file beyond size and mtime: device,
// inode and ctime. Unlike mtime, ctime cannot be set by users and changes
// with every write, so a source rewritten with its old mtime kept (rsync
// -t, touch -r) is still noticed before an interrupted transfer resumes.
func fileChangeID(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d.%09d", st.Dev, st.Ino, st.Ctim.Sec, st.Ctim.Nsec)
}
