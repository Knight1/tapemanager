package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/drive/drivetest"
)

// Whatever a drive answers, 'drive info' must print without panicking.
func FuzzPrintDriveInfo(f *testing.F) {
	f.Add([]byte{})
	f.Add(append([]byte{0, 10, 0x01, 0, 0, 0, 0, 0, 0, 0}, bytes.Repeat([]byte{0}, 40)...))
	f.Add(bytes.Repeat([]byte{0, 255}, 64))
	f.Fuzz(func(t *testing.T, b []byte) {
		info, err := drive.Gather(&drivetest.Random{Data: b})
		if err != nil {
			return
		}
		printDriveInfo(io.Discard, info, "")
	})
}
