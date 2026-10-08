package drive_test

import (
	"testing"

	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/drive/drivetest"
)

func TestReportDensitySupport(t *testing.T) {
	f := drivetest.New()
	dens, err := drive.ReportDensitySupport(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(dens) != 3 {
		t.Fatalf("%d densities", len(dens))
	}
	var lto6, lto4 *drive.Density
	for i := range dens {
		switch dens[i].PrimaryCode {
		case 0x5A:
			lto6 = &dens[i]
		case 0x46:
			lto4 = &dens[i]
		}
	}
	if lto6 == nil || !lto6.Writable || !lto6.Default || lto6.Generation != "LTO-6" || lto6.CapacityMB != 2500000 {
		t.Fatalf("lto6 %+v", lto6)
	}
	if lto4 == nil || lto4.Writable || lto4.Default || lto4.Generation != "LTO-4" {
		t.Fatalf("lto4 %+v", lto4)
	}
	if lto4.Organization != "LTO-CVE" || lto4.Description != "LTO4 800G" {
		t.Fatalf("lto4 text %+v", lto4)
	}
}

func TestCompressionEnabled(t *testing.T) {
	f := drivetest.New()
	if en, ok, err := drive.CompressionEnabled(f); err != nil || !ok || en {
		t.Fatalf("off: en=%v ok=%v err=%v", en, ok, err)
	}
	f.Compressing = true
	if en, ok, err := drive.CompressionEnabled(f); err != nil || !ok || !en {
		t.Fatalf("on: en=%v ok=%v err=%v", en, ok, err)
	}
	// A drive setting: it reads even without a cartridge.
	f.NoMedium = true
	if en, ok, err := drive.CompressionEnabled(f); err != nil || !ok || !en {
		t.Fatalf("no medium: en=%v ok=%v err=%v", en, ok, err)
	}
}
