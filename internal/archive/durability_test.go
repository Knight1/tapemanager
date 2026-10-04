package archive

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// If LTFS cannot write its index, the pending records must stay: they are
// the only copy that survives a host crash.
func TestIndexSyncFailureKeepsPendingRecords(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	syncIndex = func(string) error { return errors.New("index write failed") }
	sum, err := Put(o)
	syncIndex = realSyncIndex
	if err == nil {
		t.Fatal("put succeeded although the index could not be written")
	}
	if n, _ := pendingCount(o.Catalog.PendingPath(sum.Tape.ID)); n != 3 {
		t.Fatalf("pending records = %d, want 3", n)
	}
	var log strings.Builder
	o.Log = &log
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	if n, _ := pendingCount(o.Catalog.PendingPath(sum.Tape.ID)); n != 0 {
		t.Fatalf("pending records after recovery = %d", n)
	}
	if res := verify(t, tape); res.Verified != 3 {
		t.Fatalf("verify = %+v", res)
	}
}

// The index is forced to tape before the local records are cleared.
func TestIndexSyncedBeforePendingCleared(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	var pendingAtSync []int
	var pendingPath string
	syncIndex = func(string) error {
		n, _ := pendingCount(pendingPath)
		pendingAtSync = append(pendingAtSync, n)
		return nil
	}
	defer func() { syncIndex = realSyncIndex }()
	tp, _ := openTapeVolume(t, tape, o)
	pendingPath = o.Catalog.PendingPath(tp)
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	if len(pendingAtSync) != 1 || pendingAtSync[0] != 3 {
		t.Fatalf("pending records seen at index sync = %v", pendingAtSync)
	}
}

// After a host crash LTFS may roll back a file that was already recorded
// locally. It must be archived again, never recorded as present.
func TestRolledBackFileIsArchivedAgain(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	testHook = func(stage string, _ int64) error {
		if stage == "flush" {
			return errCrash
		}
		return nil
	}
	Put(o)
	testHook = nil
	// Simulate the rollback: the completed file is gone from the tape.
	os.Remove(filepath.Join(tape, "downloads", "sub", "b.tar"))

	var log strings.Builder
	o.Log = &log
	sum, err := Put(o)
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "no longer complete on tape") || sum.Files != 1 || sum.Skipped != 2 {
		t.Fatalf("sum = %+v\n%s", sum, log.String())
	}
	if res := verify(t, tape); res.Verified != 3 || res.Failed != 0 {
		t.Fatalf("verify = %+v", res)
	}
}

func openTapeVolume(t *testing.T, tape string, o PutOptions) (string, error) {
	t.Helper()
	// Initialize the volume up front so its ID is known.
	sum, err := Put(PutOptions{TapeRoot: tape, Source: t.TempDir(), Prefix: "init", Catalog: o.Catalog, Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	return sum.Tape.ID, nil
}
