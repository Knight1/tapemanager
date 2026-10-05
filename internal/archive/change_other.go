//go:build !linux

package archive

import "io/fs"

// fileChangeID is not available on this platform; size and mtime are compared.
func fileChangeID(fs.FileInfo) string { return "" }
