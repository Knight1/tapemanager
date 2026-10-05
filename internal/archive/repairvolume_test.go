package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Knight1/tapemanager/internal/manifest"
)

func volumeFile(tape string) string { return filepath.Join(tape, manifest.Dir, manifest.VolumeName) }

// A damaged or missing volume record blocks writing; repair-volume rebuilds
// it from the catalog only after identifying the tape, and keeps the
// damaged file.
func TestRepairVolume(t *testing.T) {
	for _, damage := range []string{"damaged", "missing"} {
		t.Run(damage, func(t *testing.T) {
			src, tape := setup(t)
			o := opts(t, src, tape)
			o.Label = "T-1"
			if _, err := Put(o); err != nil {
				t.Fatal(err)
			}
			id := loadVolumeID(t, tape)
			ro := VolumeRepairOptions{TapeRoot: tape, Catalog: o.Catalog}
			if _, err := PlanVolumeRepair(ro); err == nil || !strings.Contains(err.Error(), "fine") {
				t.Fatalf("healthy record: %v", err)
			}

			if damage == "damaged" {
				os.WriteFile(volumeFile(tape), []byte(`{"id":"garbage`), 0o644)
			} else {
				os.Remove(volumeFile(tape))
			}
			writeFile(t, filepath.Join(src, "new"), "n")
			_, err := Put(o)
			if err == nil || !strings.Contains(err.Error(), "repair-volume") {
				t.Fatalf("put on a tape without its record: %v", err)
			}
			if damage == "missing" && !errors.Is(err, manifest.ErrVolumeMissing) {
				t.Fatalf("err = %v", err)
			}
			if _, err := os.Stat(volumeFile(tape)); damage == "missing" && err == nil {
				t.Fatal("put created a new identity")
			}

			plan, err := PlanVolumeRepair(ro)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Volume.ID != id || plan.Volume.Label != "T-1" || !plan.Matched || plan.Damaged != (damage == "damaged") {
				t.Fatalf("plan = %+v", plan)
			}
			if err := plan.Execute(); err != nil {
				t.Fatal(err)
			}
			if got := loadVolumeID(t, tape); got != id {
				t.Fatalf("rebuilt ID %s, want %s", got, id)
			}
			if b, err := os.ReadFile(volumeFile(tape) + ".damaged"); (damage == "damaged") != (err == nil) || (err == nil && string(b) != `{"id":"garbage`) {
				t.Fatalf("damaged copy: %q %v", b, err)
			}
			if _, err := Put(o); err != nil {
				t.Fatalf("put after repair: %v", err)
			}
			if res := verify(t, tape); res.Verified != 4 {
				t.Fatalf("verify = %+v", res)
			}
		})
	}
}

// The identity is never guessed: an empty tape, an unknown tape and a tape
// whose files belong to another cataloged tape are refused.
func TestRepairVolumeRefuses(t *testing.T) {
	_, empty := setup(t)
	if _, err := PlanVolumeRepair(VolumeRepairOptions{TapeRoot: empty, Catalog: newCatalog(t, empty)}); err == nil || !strings.Contains(err.Error(), "no tapemgr records") {
		t.Fatalf("empty tape: %v", err)
	}

	srcA, tapeA := setup(t)
	srcB, tapeB := setup(t)
	writeFile(t, filepath.Join(srcB, "only-b"), "b")
	oA := opts(t, srcA, tapeA)
	if _, err := Put(oA); err != nil {
		t.Fatal(err)
	}
	oB := opts(t, srcB, tapeB)
	oB.Catalog = oA.Catalog
	if _, err := Put(oB); err != nil {
		t.Fatal(err)
	}
	idA := loadVolumeID(t, tapeA)
	os.Remove(volumeFile(tapeB))

	// A fresh catalog does not know the tape.
	if _, err := PlanVolumeRepair(VolumeRepairOptions{TapeRoot: tapeB, Catalog: newCatalog(t, t.TempDir()+"/x")}); err == nil || !strings.Contains(err.Error(), "--id") {
		t.Fatalf("unknown tape: %v", err)
	}
	// Tape B's files are not tape A's.
	if _, err := PlanVolumeRepair(VolumeRepairOptions{TapeRoot: tapeB, Catalog: oA.Catalog, ID: idA}); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("wrong --id: %v", err)
	}
	if _, err := PlanVolumeRepair(VolumeRepairOptions{TapeRoot: tapeB, Catalog: oA.Catalog, ID: "00000000-0000-4000-8000-000000000009"}); err == nil {
		t.Fatal("unknown --id accepted")
	}
}
