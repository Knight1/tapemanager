//go:build !linux

package ltfs

// CheckMounted is a no-op on platforms without a known LTFS mount signature.
func CheckMounted(path string) error { return nil }
