package archive

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Knight1/tapemanager/internal/manifest"
	"github.com/Knight1/tapemanager/internal/parity"
)

const (
	pChunk  = 16
	pWindow = parity.K * parity.D * pChunk // bytes per parity window
)

// paritySetup creates a large file spanning three and a half parity windows
// and a small one, with 10% parity (two parity shards per stripe).
func paritySetup(t *testing.T) (src, tape string, big, small []byte, o PutOptions) {
	t.Helper()
	base := t.TempDir()
	src = filepath.Join(base, "src")
	tape = filepath.Join(base, "tape")
	big = make([]byte, 3*pWindow+pWindow/2+7)
	small = make([]byte, 200)
	rand.Read(big)
	rand.Read(small)
	writeFile(t, filepath.Join(src, "big.bin"), string(big))
	writeFile(t, filepath.Join(src, "small.bin"), string(small))
	os.MkdirAll(tape, 0o755)
	o = opts(t, src, tape)
	o.ChunkSize = pChunk
	o.CheckpointEvery = 64
	o.Parity = 10
	return
}

func damage(t *testing.T, path string, off int64, n int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	junk := bytes.Repeat([]byte{0xAA}, n)
	if _, err := f.WriteAt(junk, off); err != nil {
		t.Fatal(err)
	}
}

func restoreAll(t *testing.T, tape string) (RestoreResult, string) {
	t.Helper()
	dest := filepath.Join(filepath.Dir(tape), "restored")
	os.RemoveAll(dest)
	var log strings.Builder
	res, err := Restore(RestoreOptions{TapeRoot: tape, Dest: dest, Log: &log})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	return res, dest
}

func checkRestored(t *testing.T, dest string, big, small []byte) {
	t.Helper()
	for name, want := range map[string][]byte{"big.bin": big, "small.bin": small} {
		got, err := os.ReadFile(filepath.Join(dest, "src", name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s restored wrong (%d bytes, %v)", name, len(got), err)
		}
	}
}

func TestParityWrittenToSegment(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	tp, _ := manifest.Open(tape)
	defer tp.Close()
	par, err := tp.Parity()
	if err != nil {
		t.Fatal(err)
	}
	b, s := par["src/big.bin"], par["src/small.bin"]
	if b.Layout.Scheme != parity.SchemeWindow || s.Layout.Scheme != parity.SchemeSmall || b.Layout.M != 2 {
		t.Fatalf("layouts: %+v / %+v", b.Layout, s.Layout)
	}
	blob, _ := os.Stat(filepath.Join(tape, manifest.Dir, manifest.SegmentsDir, "000001.parity"))
	want := b.Layout.ParitySize() + s.Layout.ParitySize()
	if blob == nil || blob.Size() != want {
		t.Fatalf("parity file size %v, want %d", blob, want)
	}
	// Overhead stays near 10% for both files.
	if r := float64(b.Layout.ParitySize()) / float64(len(big)); r < 0.09 || r > 0.12 {
		t.Errorf("large file overhead %.3f", r)
	}
	if r := float64(s.Layout.ParitySize()) / float64(len(small)); r < 0.09 || r > 0.12 {
		t.Errorf("small file overhead %.3f", r)
	}
	if staged, _ := filepath.Glob(filepath.Join(o.Catalog.Dir, "parity", "*", "*")); len(staged) != 0 {
		t.Errorf("staged parity left behind: %v", staged)
	}
	if res := verify(t, tape); res.Verified != 2 || res.Repairable != 0 {
		t.Fatalf("verify = %+v", res)
	}
}

// One contiguous damaged area of M*D chunks inside a window is rebuilt.
func TestParityRepairsBurst(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	Put(o)
	damage(t, filepath.Join(tape, "src", "big.bin"), pWindow+5*pChunk, 2*parity.D*pChunk)
	damage(t, filepath.Join(tape, "src", "small.bin"), 0, 10)

	var log strings.Builder
	res, err := Verify(VerifyOptions{TapeRoot: tape, Catalog: o.Catalog, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if res.Repairable != 2 || res.Failed != 0 || !strings.Contains(log.String(), "repairable with parity") {
		t.Fatalf("verify = %+v\n%s", res, log.String())
	}
	got, dest := restoreAll(t, tape)
	if got.Files != 2 || got.Repaired != 2 || got.Failed != 0 {
		t.Fatalf("restore = %+v", got)
	}
	checkRestored(t, dest, big, small)
}

// A truncated file stands in for unreadable tape: reads past the cut fail.
func TestParityRepairsUnreadableTail(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	Put(o)
	os.Truncate(filepath.Join(tape, "src", "big.bin"), int64(len(big))-20)

	var log strings.Builder
	res, _ := Verify(VerifyOptions{TapeRoot: tape, Log: &log})
	if res.Repairable != 1 || !strings.Contains(log.String(), "unreadable bytes") {
		t.Fatalf("verify = %+v\n%s", res, log.String())
	}
	_, dest := restoreAll(t, tape)
	checkRestored(t, dest, big, small)
}

func TestParityTooMuchDamage(t *testing.T) {
	_, tape, _, _, o := paritySetup(t)
	Put(o)
	// Three chunks of one stripe (stride D) exceed two parity shards.
	p := filepath.Join(tape, "src", "big.bin")
	for _, c := range []int64{0, parity.D, 2 * parity.D} {
		damage(t, p, c*pChunk, 1)
	}
	res, _ := Verify(VerifyOptions{TapeRoot: tape, Log: io.Discard})
	if res.Failed != 1 || res.Repairable != 0 {
		t.Fatalf("verify = %+v", res)
	}
	got, dest := restoreAll(t, tape)
	if got.Failed != 1 || got.Files != 1 {
		t.Fatalf("restore = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "src", "big.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("damaged file left in the destination")
	}
	if left, _ := filepath.Glob(filepath.Join(dest, "src", "*"+PartialSuffix)); len(left) != 0 {
		t.Fatalf("partial restore left behind: %v", left)
	}
}

// Damaged parity is detected by its hash and treated as missing, so the
// second parity shard still repairs the data.
func TestParityDamagedParityShard(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	Put(o)
	damage(t, filepath.Join(tape, manifest.Dir, manifest.SegmentsDir, "000001.parity"), 0, 4)
	damage(t, filepath.Join(tape, "src", "big.bin"), 0, 4)
	_, dest := restoreAll(t, tape)
	checkRestored(t, dest, big, small)
}

func TestParityResumeMidFile(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	// Checkpoints wait for window boundaries; crash after the second.
	crashAt(t, "checkpoint", 2*pWindow)
	if _, err := Put(o); !errors.Is(err, errCrash) {
		t.Fatalf("err = %v", err)
	}
	testHook = nil
	sum, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 2*pWindow {
		t.Fatalf("resumed at %d, want %d", sum.Resumed, 2*pWindow)
	}
	// Parity from before and after the resume must both work: damage the
	// first and the last window.
	p := filepath.Join(tape, "src", "big.bin")
	damage(t, p, 3*pChunk, pChunk)
	damage(t, p, 3*pWindow+pChunk, pChunk)
	_, dest := restoreAll(t, tape)
	checkRestored(t, dest, big, small)
}

func TestParityResumeRestartsWithoutStagedParity(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	crashAt(t, "checkpoint", pWindow)
	Put(o)
	testHook = nil
	os.RemoveAll(filepath.Join(o.Catalog.Dir, "parity"))
	sum, err := Put(o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 0 {
		t.Fatalf("resumed at %d without staged parity", sum.Resumed)
	}
	damage(t, filepath.Join(tape, "src", "big.bin"), 0, pChunk)
	_, dest := restoreAll(t, tape)
	checkRestored(t, dest, big, small)
}

func TestParityAfterRenameBeforeRecord(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	crashAt(t, "manifest", 0)
	Put(o)
	testHook = nil
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	damage(t, filepath.Join(tape, "src", "big.bin"), 0, pChunk)
	_, dest := restoreAll(t, tape)
	checkRestored(t, dest, big, small)
}

func TestParityRecoveredFromPending(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	testHook = func(stage string, _ int64) error {
		if stage == "flush" {
			return errCrash
		}
		return nil
	}
	Put(o)
	testHook = nil
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	damage(t, filepath.Join(tape, "src", "small.bin"), 5, 3)
	_, dest := restoreAll(t, tape)
	checkRestored(t, dest, big, small)
}

func TestParityLostStagingIsAWarning(t *testing.T) {
	_, tape, _, _, o := paritySetup(t)
	testHook = func(stage string, _ int64) error {
		if stage == "flush" {
			return errCrash
		}
		return nil
	}
	Put(o)
	testHook = nil
	os.RemoveAll(filepath.Join(o.Catalog.Dir, "parity"))
	var log strings.Builder
	o.Log = &log
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "archived without parity") {
		t.Fatalf("no warning:\n%s", log.String())
	}
	if res := verify(t, tape); res.Verified != 2 {
		t.Fatalf("verify = %+v", res)
	}
}

func TestParityDisabled(t *testing.T) {
	_, tape, _, _, o := paritySetup(t)
	o.Parity = 0
	Put(o)
	tp, _ := manifest.Open(tape)
	defer tp.Close()
	if par, _ := tp.Parity(); len(par) != 0 {
		t.Fatalf("parity written although disabled: %d", len(par))
	}
	o.Parity = 30
	if _, err := Put(o); err == nil {
		t.Fatal("parity above the maximum accepted")
	}
}

func TestRepairableBlocksPurge(t *testing.T) {
	src, tape, _, _, o := paritySetup(t)
	Put(o)
	damage(t, filepath.Join(tape, "src", "big.bin"), 0, 4)
	Verify(VerifyOptions{TapeRoot: tape, Catalog: o.Catalog, Log: io.Discard})
	if p := planPurge(t, src, tape, o.Catalog, false); len(p.Delete) != 0 {
		t.Fatal("purge allowed from a damaged tape")
	}
}

func TestRestoreNeverOverwrites(t *testing.T) {
	_, tape, _, _, o := paritySetup(t)
	Put(o)
	dest := filepath.Join(filepath.Dir(tape), "restored")
	writeFile(t, filepath.Join(dest, "src", "small.bin"), "mine")
	res, err := Restore(RestoreOptions{TapeRoot: tape, Dest: dest, Log: io.Discard})
	if err != nil || res.Failed != 1 || res.Files != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "src", "small.bin")); string(got) != "mine" {
		t.Fatal("existing file overwritten")
	}
}

func TestRestorePathAndReferences(t *testing.T) {
	src, tape := setup(t)
	writeFile(t, filepath.Join(src, "copy.iso"), "alpha")
	o := opts(t, src, tape)
	o.Dedup, o.DedupMinSize = true, 1
	Put(o)

	dest := filepath.Join(filepath.Dir(tape), "out")
	res, err := Restore(RestoreOptions{TapeRoot: tape, Path: "downloads/sub", Dest: dest, Log: io.Discard})
	if err != nil || res.Files != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	res, _ = Restore(RestoreOptions{TapeRoot: tape, Path: "downloads/copy.iso", Dest: dest, Log: io.Discard})
	if res.Skipped != 1 {
		t.Fatalf("reference not skipped: %+v", res)
	}
	if _, err := Restore(RestoreOptions{TapeRoot: tape, Path: "nothing", Dest: dest, Log: io.Discard}); err == nil {
		t.Fatal("no error for an unmatched path")
	}
}

func TestStaleParityRemoved(t *testing.T) {
	_, tape, _, _, o := paritySetup(t)
	Put(o)
	sum, _ := Put(o)
	dir := o.Catalog.ParityDir(sum.Tape.ID)
	os.MkdirAll(dir, 0o755)
	stale := filepath.Join(dir, "0123456789abcdef0123456789abcdef.bin")
	os.WriteFile(stale, []byte("x"), 0o644)
	Put(o)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale staged parity kept")
	}
	_ = tape
}

func TestParityRejectsHugeChunks(t *testing.T) {
	_, _, _, _, o := paritySetup(t)
	o.ChunkSize = parity.MaxShardSize + 1
	if _, err := Put(o); err == nil {
		t.Fatal("parity with oversized chunks accepted")
	}
}
