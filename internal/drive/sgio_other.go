//go:build !linux

package drive

import "errors"

// Open is only supported on Linux.
func Open(path string) (Device, error) {
	return nil, errors.New("direct drive access is only supported on Linux")
}
