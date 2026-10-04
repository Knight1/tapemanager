package archive

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/manifest"
)

func segFile(tape, name string) string {
	return filepath.Join(tape, manifest.Dir, manifest.SegmentsDir, name)
}

// One damaged line in the parity list must not block restoring the tape.
func TestDamagedParityListStillRestores(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	Put(o)
	list := segFile(tape, "000001.parity.jsonl")
	b, _ := os.ReadFile(list)
	os.WriteFile(list, append([]byte("garbage\n"), b...), 0o644)

	var log strings.Builder
	res, err := Verify(VerifyOptions{TapeRoot: tape, Log: &log})
	if err != nil || res.Verified != 2 || res.Problems != 1 || !strings.Contains(log.String(), "damaged records skipped") {
		t.Fatalf("verify = %+v, %v\n%s", res, err, log.String())
	}
	got, dest := restoreAll(t, tape)
	if got.Files != 2 {
		t.Fatalf("restore = %+v", got)
	}
	checkRestored(t, dest, big, small)
}

// A verify that cannot finish is recorded as failed, so an older pass can
// no longer allow purging.
func TestAbortedVerifyRevokesPurge(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)
	verifyWith(t, tape, o.Catalog)
	if p := planPurge(t, src, tape, o.Catalog, false); len(p.Delete) != 3 {
		t.Fatalf("precondition: %+v", p)
	}

	// Make the segment directory unreadable as a directory.
	seg := filepath.Join(tape, manifest.Dir, manifest.SegmentsDir)
	os.Rename(seg, seg+".moved")
	os.WriteFile(seg, []byte("not a directory"), 0o644)
	if _, err := Verify(VerifyOptions{TapeRoot: tape, Catalog: o.Catalog, Log: io.Discard}); err == nil {
		t.Fatal("verify succeeded on unreadable metadata")
	}
	if p := planPurge(t, src, tape, o.Catalog, false); len(p.Delete) != 0 {
		t.Fatal("purge still allowed after an aborted verify")
	}
}

func TestMetadataProblemsRevokePurge(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)
	verifyWith(t, tape, o.Catalog)
	list := segFile(tape, "000001.chunks.jsonl")
	b, _ := os.ReadFile(list)
	os.WriteFile(list, append(b, []byte("garbage\n")...), 0o644)
	Verify(VerifyOptions{TapeRoot: tape, Catalog: o.Catalog, Log: io.Discard})
	if p := planPurge(t, src, tape, o.Catalog, false); len(p.Delete) != 0 {
		t.Fatal("purge allowed with damaged metadata")
	}
}

// A failed parity write must never cost the manifest.
func TestParityWriteFailureKeepsManifest(t *testing.T) {
	_, tape, _, _, o := paritySetup(t)
	testHook = func(stage string, _ int64) error {
		if stage == "parity-data" {
			return errCrash
		}
		return nil
	}
	defer func() { testHook = nil }()
	var log strings.Builder
	o.Log = &log
	sum, err := Put(o)
	if err != nil {
		t.Fatalf("put failed: %v", err)
	}
	if sum.Files != 2 || !strings.Contains(log.String(), "have no parity") {
		t.Fatalf("summary = %+v\n%s", sum, log.String())
	}
	if entries, _ := loadEntries(tape); len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	testHook = nil
	if res := verify(t, tape); res.Verified != 2 {
		t.Fatalf("verify = %+v", res)
	}
}

func TestTapeFull(t *testing.T) {
	src, tape := setup(t)
	writeFile(t, filepath.Join(src, "z-last.bin"), strings.Repeat("z", 1000))
	o := opts(t, src, tape)
	// Room for the small files and the metadata reserve, not the last one.
	// The last file needs 1000 bytes plus the reserve for 4 entries.
	reserve := metadataReserve(4, 1010, o.ChunkSizeOrDefault())
	freeSpace = func(p string) (int64, error) {
		if p == tape {
			return reserve + 100, nil
		}
		return 1 << 40, nil
	}
	defer func() { freeSpace = realFreeSpace }()

	sum, err := Put(o)
	if !errors.Is(err, ErrTapeFull) || sum.Files != 3 {
		t.Fatalf("sum = %+v, err = %v", sum, err)
	}
	if entries, _ := loadEntries(tape); len(entries) != 3 {
		t.Fatalf("entries recorded before the full tape = %d", len(entries))
	}

	// The next tape gets only the file that did not fit.
	tape2 := filepath.Join(filepath.Dir(tape), "tape2")
	os.MkdirAll(tape2, 0o755)
	o.TapeRoot = tape2
	sum, err = Put(o)
	if err != nil || sum.Files != 1 || sum.Elsewhere != 3 {
		t.Fatalf("next tape: %+v, %v", sum, err)
	}
}

func TestCatalogDiskFull(t *testing.T) {
	_, _, _, _, o := paritySetup(t)
	freeSpace = func(p string) (int64, error) {
		if p == o.Catalog.Dir {
			return 1 << 20, nil
		}
		return 1 << 40, nil
	}
	defer func() { freeSpace = realFreeSpace }()
	if _, err := Put(o); err == nil || !strings.Contains(err.Error(), "catalog disk") {
		t.Fatalf("err = %v", err)
	}
}

// A crafted size far beyond the file on tape must not make verify loop.
func TestCraftedHugeSizeIsFast(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	m := segFile(tape, "000001.manifest.jsonl")
	b, _ := os.ReadFile(m)
	os.WriteFile(m, []byte(strings.Replace(string(b), `"size":5,`, `"size":1099511627776,`, 1)), 0o644)

	start := time.Now()
	var log strings.Builder
	res, err := Verify(VerifyOptions{TapeRoot: tape, Log: &log})
	if err != nil || res.Failed != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("res = %+v, err = %v, took %v", res, err, time.Since(start))
	}
	if !strings.Contains(log.String(), "cut short") {
		t.Fatalf("log:\n%s", log.String())
	}
}

func TestDestroyedParityIsReported(t *testing.T) {
	_, tape, _, _, o := paritySetup(t)
	Put(o)
	blob := segFile(tape, "000001.parity")
	st, _ := os.Stat(blob)
	os.WriteFile(blob, make([]byte, st.Size()), 0o644)
	var log strings.Builder
	res, _ := Verify(VerifyOptions{TapeRoot: tape, Catalog: o.Catalog, Log: &log})
	if res.Verified != 2 || res.Problems != 2 || !strings.Contains(log.String(), "parity of src/big.bin") {
		t.Fatalf("res = %+v\n%s", res, log.String())
	}
	tp, _ := o.Catalog.Tapes()
	if tp[0].LastVerified().Passed() {
		t.Fatal("verification with damaged parity recorded as passed")
	}
}

// When the newest checkpoint cannot be used, an older one is.
func TestResumeFallsBackToOlderCheckpoint(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	crashAt(t, "checkpoint", 2*pWindow)
	Put(o)
	testHook = nil
	// Damage staged parity of the second window only.
	staged, _ := filepath.Glob(filepath.Join(o.Catalog.Dir, "parity", "*", "*.bin"))
	if len(staged) != 1 {
		t.Fatalf("staged = %v", staged)
	}
	secondWindow := int64(16*2) * pChunk // D*M shards of the first window
	damage(t, staged[0], secondWindow+3, 1)

	sum, err := Put(o)
	if err != nil || sum.Resumed != pWindow {
		t.Fatalf("resumed at %d, want %d (%v)", sum.Resumed, pWindow, err)
	}
	damage(t, filepath.Join(tape, "src", "big.bin"), pWindow+pChunk, pChunk)
	_, dest := restoreAll(t, tape)
	checkRestored(t, dest, big, small)
}

func TestSmallShaMismatchIsNotRepairable(t *testing.T) {
	_, tape, _, small, o := paritySetup(t)
	Put(o)
	m := segFile(tape, "000001.manifest.jsonl")
	b, _ := os.ReadFile(m)
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		if strings.Contains(l, "small.bin") {
			idx := strings.Index(l, `"sha256":"`) + len(`"sha256":"`)
			c := byte('0')
			if l[idx] == '0' {
				c = '1'
			}
			lines[i] = l[:idx] + string(c) + l[idx+1:]
		}
	}
	os.WriteFile(m, []byte(strings.Join(lines, "\n")), 0o644)
	res, _ := Verify(VerifyOptions{TapeRoot: tape, Log: io.Discard})
	if res.Repairable != 0 || res.Failed != 1 {
		t.Fatalf("res = %+v", res)
	}
	_ = small
}

func TestRestoreWithoutHardLinks(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	destDir := filepath.Join(filepath.Dir(tape), "fat")
	os.MkdirAll(filepath.Join(destDir, "downloads"), 0o755)
	os.WriteFile(filepath.Join(destDir, "downloads", "a.iso"), []byte("mine"), 0o644)
	dest, _ := os.OpenRoot(destDir)
	defer dest.Close()
	tp, _ := manifest.Open(tape)
	defer tp.Close()
	entries, _ := tp.Entries()
	for _, e := range entries {
		_, err := restoreFile(tp, dest, false, e, nil, nil, nil)
		switch e.Path {
		case "downloads/a.iso":
			if err == nil {
				t.Error("existing file overwritten")
			}
		default:
			if err != nil {
				t.Errorf("%s: %v", e.Path, err)
			}
		}
	}
	if b, _ := os.ReadFile(filepath.Join(destDir, "downloads", "a.iso")); string(b) != "mine" {
		t.Fatal("existing file changed")
	}
	if b, _ := os.ReadFile(filepath.Join(destDir, "downloads", "sub", "b.tar")); string(b) != "bravo" {
		t.Fatal("file not restored")
	}
}

// Restore never touches files it did not create, whatever their names.
func TestRestoreLeavesForeignFilesAlone(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	dest := filepath.Join(filepath.Dir(tape), "out")
	foreign := filepath.Join(dest, "downloads", "a.iso"+PartialSuffix)
	writeFile(t, foreign, "mine")
	res, err := Restore(RestoreOptions{TapeRoot: tape, Dest: dest, Log: io.Discard})
	if err != nil || res.Files != 3 || res.Failed != 0 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "mine" {
		t.Fatal("foreign file touched")
	}
	if left, _ := filepath.Glob(filepath.Join(dest, "downloads", "*.tapemgr-restore-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// A file renamed into place but not recorded is finished even if the rerun
// uses different settings.
func TestRenamedFileFinishedWithOtherSettings(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	o.Parity = 0
	crashAt(t, "manifest", 0)
	Put(o)
	testHook = nil
	o.Parity = 10
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	if res := verify(t, tape); res.Verified != 2 {
		t.Fatalf("verify = %+v", res)
	}
	_, dest := restoreAll(t, tape)
	checkRestored(t, dest, big, small)
}

// A damaged volume record must not leave an older pass in place.
func TestDamagedVolumeStillRecordsVerify(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)
	verifyWith(t, tape, o.Catalog)
	os.WriteFile(filepath.Join(tape, manifest.Dir, manifest.VolumeName), []byte("{garbage"), 0o644)
	damage(t, filepath.Join(tape, "downloads", "a.iso"), 0, 1)

	res, _ := Verify(VerifyOptions{TapeRoot: tape, Catalog: o.Catalog, Log: io.Discard})
	if res.Failed != 1 || res.Problems < 1 {
		t.Fatalf("res = %+v", res)
	}
	if p := planPurge(t, src, tape, o.Catalog, false); len(p.Delete) != 0 {
		t.Fatal("purge allowed after a failed verify of a tape with a damaged volume record")
	}
}

func TestUnidentifiedTapeReportsUnrecorded(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	put(t, src, tape)
	os.WriteFile(filepath.Join(tape, manifest.Dir, manifest.VolumeName), []byte("{garbage"), 0o644)
	empty := newCatalog(t, filepath.Join(t.TempDir(), "x"))
	_, err := Verify(VerifyOptions{TapeRoot: tape, Catalog: empty, Log: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "NOT recorded") {
		t.Fatalf("err = %v", err)
	}
	_ = o
}

// A process dying during the parity write must not lose that parity.
func TestCrashDuringParityWriteKeepsParity(t *testing.T) {
	_, tape, big, small, o := paritySetup(t)
	testHook = func(stage string, _ int64) error {
		if stage == "parity-data" {
			panic(errCrash)
		}
		return nil
	}
	func() {
		defer func() { recover() }()
		Put(o)
	}()
	testHook = nil
	if entries, _ := loadEntries(tape); len(entries) != 2 {
		t.Fatalf("precondition: manifest has %d entries", len(entries))
	}
	var log strings.Builder
	o.Log = &log
	if _, err := Put(o); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	tp, _ := manifest.Open(tape)
	par, _, _ := tp.Parity()
	tp.Close()
	if len(par) != 2 {
		t.Fatalf("parity records after recovery = %d\n%s", len(par), log.String())
	}
	damage(t, filepath.Join(tape, "src", "big.bin"), 0, pChunk)
	damage(t, filepath.Join(tape, "src", "small.bin"), 0, 3)
	_, dest := restoreAll(t, tape)
	checkRestored(t, dest, big, small)
}

func TestNextTapeContinuesAfterFullTape(t *testing.T) {
	src, tape := setup(t)
	writeFile(t, filepath.Join(src, "z-last.bin"), strings.Repeat("z", 1000))
	o := opts(t, src, tape)
	reserve := metadataReserve(4, 1010, o.ChunkSizeOrDefault())
	freeSpace = func(p string) (int64, error) {
		if p == tape {
			return reserve + 100, nil
		}
		return 1 << 40, nil
	}
	defer func() { freeSpace = realFreeSpace }()
	if _, err := Put(o); !errors.Is(err, ErrTapeFull) {
		t.Fatalf("err = %v", err)
	}
	tape2 := filepath.Join(filepath.Dir(tape), "tape2")
	os.MkdirAll(tape2, 0o755)
	o.TapeRoot = tape2
	var log strings.Builder
	o.Log = &log
	if sum, err := Put(o); err != nil || sum.Elsewhere != 3 || sum.Files != 1 || !strings.Contains(log.String(), "already on tape") {
		t.Fatalf("next tape: %+v, %v\n%s", sum, err, log.String())
	}
	// Everything has one copy now; a third tape gets nothing new.
	tape3 := filepath.Join(filepath.Dir(tape), "tape3")
	os.MkdirAll(tape3, 0o755)
	o.TapeRoot = tape3
	o.Log = io.Discard
	if sum, err := Put(o); err != nil || sum.Files != 0 || sum.Elsewhere != 4 {
		t.Fatalf("third tape: %+v, %v", sum, err)
	}
}

// A file already renamed into place on a nearly full tape is finished, not
// refused as not fitting.
func TestRenamedFileOnFullTapeIsRecorded(t *testing.T) {
	src, tape := setup(t)
	writeFile(t, filepath.Join(src, "big.bin"), strings.Repeat("b", 5000))
	o := opts(t, src, tape)
	crashAt(t, "manifest", 4000)
	Put(o)
	testHook = nil
	freeSpace = func(p string) (int64, error) {
		if p == tape {
			return metadataReserve(10, 6000, o.ChunkSizeOrDefault()) + 100, nil
		}
		return 1 << 40, nil
	}
	defer func() { freeSpace = realFreeSpace }()
	// a.iso was recorded before the crash; big.bin is finished from its
	// journal without needing space again.
	sum, err := Put(o)
	if err != nil || sum.Files != 3 || sum.Skipped != 1 || sum.Resumed != 5000 {
		t.Fatalf("sum = %+v, err = %v", sum, err)
	}
}

func TestDamagedWrittenLogDoesNotBlockPut(t *testing.T) {
	src, tape := setup(t)
	o := opts(t, src, tape)
	sum := put(t, src, tape)
	w := filepath.Join(o.Catalog.Dir, "written", sum.Tape.ID+".jsonl")
	b, _ := os.ReadFile(w)
	os.WriteFile(w, append([]byte("garbage\n"), b...), 0o644)
	tape2 := filepath.Join(filepath.Dir(tape), "tape2")
	os.MkdirAll(tape2, 0o755)
	o.TapeRoot = tape2
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
}

func TestStaleSumsRewritten(t *testing.T) {
	src, tape := setup(t)
	put(t, src, tape)
	sums := filepath.Join(tape, manifest.SumsName)
	os.WriteFile(sums, []byte("stale\n"), 0o644)
	put(t, src, tape)
	b, _ := os.ReadFile(sums)
	if strings.Contains(string(b), "stale") || strings.Count(string(b), "\n") != 3 {
		t.Fatalf("SHA256SUMS = %q", b)
	}
}
