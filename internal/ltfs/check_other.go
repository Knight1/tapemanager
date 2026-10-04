//go:build !linux

package ltfs

// CheckMounted is a no-op on platforms without a known LTFS mount signature.
func CheckMounted(path string) error { return nil }

// VolumeUUID is not supported on this platform.
func VolumeUUID(path string) string { return "" }

// StartBlock is not supported on this platform.
func StartBlock(path string) (int64, bool) { return 0, false }

// FreeSpace is unknown on this platform and reported as unlimited.
func FreeSpace(path string) (int64, error) { return 1 << 62, nil }

// SyncIndex is a no-op on this platform.
func SyncIndex(root string) error { return nil }

// ReadOnly is not detected on this platform; writes fail if it is.
func ReadOnly(path string) (bool, error) { return false, nil }
