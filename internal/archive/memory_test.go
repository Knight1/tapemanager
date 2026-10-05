package archive

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Knight1/tapemanager/internal/memcheck"
)

// limitMemory makes every allocation above limit fail the memory check.
func limitMemory(t *testing.T, limit int64) *[]string {
	var asked []string
	ensureMemory = func(need int64, what string) error {
		asked = append(asked, fmt.Sprintf("%s:%d", what, need))
		if need > limit {
			return fmt.Errorf("%w: %s", memcheck.ErrNoMemory, what)
		}
		return nil
	}
	t.Cleanup(func() { ensureMemory = memcheck.Ensure })
	return &asked
}

// Too little memory stops put before a file is started, and verify
// without counting anything as damage or recording a result.
func TestNotEnoughMemory(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	o.Parity = 10
	asked := limitMemory(t, 1<<10)
	_, err := Put(o)
	if !errors.Is(err, memcheck.ErrNoMemory) || !strings.Contains(err.Error(), "archiving") || len(*asked) == 0 {
		t.Fatalf("put: %v (%v)", err, *asked)
	}
	if ents, _ := filepath.Glob(filepath.Join(tape, "downloads", "*")); len(ents) != 0 {
		t.Fatalf("files started without memory: %v", ents)
	}

	ensureMemory = memcheck.Ensure
	put(t, src, tape)
	verifyOpts := VerifyOptions{TapeRoot: tape, Catalog: o.Catalog, Log: io.Discard}
	if _, err := Verify(verifyOpts); err != nil {
		t.Fatal(err)
	}
	limitMemory(t, 1<<10)
	res, err := Verify(verifyOpts)
	if !errors.Is(err, memcheck.ErrNoMemory) || res.Failed != 0 {
		t.Fatalf("verify: %+v %v", res, err)
	}
	tapes, _ := o.Catalog.Tapes()
	if len(tapes) != 1 || len(tapes[0].Verifications) != 1 || !tapes[0].Verifications[0].Passed() {
		t.Fatalf("a memory shortage was recorded: %+v", tapes)
	}

	// Restore fails only the file that does not fit.
	rres, err := Restore(RestoreOptions{TapeRoot: tape, Dest: t.TempDir(), Log: io.Discard})
	if err != nil || rres.Failed == 0 {
		t.Fatalf("restore: %+v %v", rres, err)
	}
}
