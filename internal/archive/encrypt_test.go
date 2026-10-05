package archive

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/Knight1/tapemanager/internal/manifest"
)

func ageIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// encSetup creates a source with an empty file, a small one and one that
// spans several age chunks and many tape chunks.
func encSetup(t *testing.T) (src, tape string, big []byte) {
	src, tape = setup(t)
	big = make([]byte, 300<<10)
	rand.Read(big)
	writeFile(t, filepath.Join(src, "big.bin"), string(big))
	return src, tape, big
}

func encOpts(t *testing.T, src, tape string, id *age.X25519Identity) PutOptions {
	o := opts(t, src, tape)
	o.Recipients = []age.Recipient{id.Recipient()}
	// No parity by default: with parity, checkpoints wait for a parity
	// window boundary, and these files are smaller than a window.
	o.ChunkSize = 16 << 10
	o.CheckpointEvery = 32 << 10
	return o
}

func decryptFile(t *testing.T, path string, ids ...age.Identity) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := age.Decrypt(f, ids...)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestEncryptedPutVerifyRestore(t *testing.T) {
	src, tape, big := encSetup(t)
	id := ageIdentity(t)
	o := encOpts(t, src, tape, id)
	o.Parity = 10
	sum, err := Put(o)
	if err != nil || sum.Files != 4 {
		t.Fatalf("%+v %v", sum, err)
	}

	want := map[string][]byte{"a.iso": []byte("alpha"), "sub/b.tar": []byte("bravo"), "empty": nil, "big.bin": big}
	entries, _ := loadEntries(tape)
	sums, _ := os.ReadFile(filepath.Join(tape, manifest.SumsName))
	for _, e := range entries {
		rel := strings.TrimPrefix(e.Path, "downloads/")
		plain := want[rel]
		if e.Age == nil || e.Size != int64(len(plain)) || e.SHA256 != sha(string(plain)) || e.Age.Recipients[0] != id.Recipient().String() {
			t.Fatalf("entry %+v", e)
		}
		// No plaintext on tape; the age file decrypts with the age library.
		if exists(filepath.Join(tape, e.Path)) {
			t.Fatalf("%s stored unencrypted", e.Path)
		}
		stored := filepath.Join(tape, e.Path+".age")
		if got := decryptFile(t, stored, id); !bytes.Equal(got, plain) {
			t.Fatalf("%s decrypts to different data", e.Path)
		}
		// SHA256SUMS lists the encrypted file, so sha256sum -c works.
		st, _ := os.Stat(stored)
		if st.Size() != e.Age.Size || !strings.Contains(string(sums), fileSHA(t, stored)+"  "+e.Path+".age\n") {
			t.Fatalf("%s: size %d/%d or SHA256SUMS line missing", e.Path, st.Size(), e.Age.Size)
		}
	}

	// Verification needs no key.
	if res := verify(t, tape); res.Verified != 4 || res.Failed != 0 || res.Problems != 0 {
		t.Fatalf("verify: %+v", res)
	}

	dest := t.TempDir()
	res, err := Restore(RestoreOptions{TapeRoot: tape, Dest: dest, Log: io.Discard, Identities: []age.Identity{id}})
	if err != nil || res.Files != 4 || res.Failed != 0 {
		t.Fatalf("restore: %+v %v", res, err)
	}
	for rel, plain := range want {
		got, err := os.ReadFile(filepath.Join(dest, "downloads", rel))
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("restored %s differs: %v", rel, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dest, "downloads", "*.tapemgr-restore-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}

	// Without a key, or with the wrong one, nothing is restored.
	var log strings.Builder
	res, _ = Restore(RestoreOptions{TapeRoot: tape, Dest: t.TempDir(), Log: &log})
	if res.Failed != 4 || !strings.Contains(log.String(), "pass --identity") {
		t.Fatalf("no key: %+v\n%s", res, log.String())
	}
	log.Reset()
	other := t.TempDir()
	res, _ = Restore(RestoreOptions{TapeRoot: tape, Dest: other, Log: &log, Identities: []age.Identity{ageIdentity(t)}})
	if res.Failed != 4 || !strings.Contains(log.String(), "none of the given keys") {
		t.Fatalf("wrong key: %+v\n%s", res, log.String())
	}
	filepath.WalkDir(other, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			t.Errorf("file left after failed decryption: %s", p)
		}
		return nil
	})
}

func TestEncryptedResume(t *testing.T) {
	src, tape, big := encSetup(t)
	id := ageIdentity(t)
	o := encOpts(t, src, tape, id)
	for _, at := range []int64{32 << 10, 96 << 10, 200 << 10} {
		crashAt(t, "checkpoint", at)
		if _, err := Put(o); !errors.Is(err, errCrash) {
			t.Fatalf("crash at %d: %v", at, err)
		}
	}
	testHook = nil
	sum, err := Put(o)
	if err != nil || sum.Resumed == 0 {
		t.Fatalf("%+v %v", sum, err)
	}
	if got := decryptFile(t, filepath.Join(tape, "downloads", "big.bin.age"), id); !bytes.Equal(got, big) {
		t.Fatal("resumed file decrypts to different data")
	}
	if res := verify(t, tape); res.Verified != 4 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
	entries, _ := loadEntries(tape)
	for _, e := range entries {
		if e.Path == "downloads/big.bin" && e.SHA256 != sha(string(big)) {
			t.Fatal("wrong plaintext hash after resume")
		}
	}
}

// The source changed after the interruption, keeping size and mtime: the
// write starts over with a new key instead of mixing versions.
func TestEncryptedResumeSourceChanged(t *testing.T) {
	src, tape, big := encSetup(t)
	id := ageIdentity(t)
	o := encOpts(t, src, tape, id)
	path := filepath.Join(src, "big.bin")
	crashAt(t, "checkpoint", 100<<10)
	Put(o)
	testHook = nil

	st, _ := os.Stat(path)
	changed := bytes.Clone(big)
	for i := 90 << 10; i < 110<<10; i++ {
		changed[i] ^= 0xff
	}
	os.WriteFile(path, changed, 0o644)
	os.Chtimes(path, st.ModTime(), st.ModTime())

	var log strings.Builder
	o.Log = &log
	sum, err := Put(o)
	if err != nil || !strings.Contains(log.String(), "started over") {
		t.Fatalf("%+v %v\n%s", sum, err, log.String())
	}
	if got := decryptFile(t, filepath.Join(tape, "downloads", "big.bin.age"), id); !bytes.Equal(got, changed) {
		t.Fatal("file does not hold the new version")
	}
	if res := verify(t, tape); res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
}

// Renamed into place, then interrupted before the record was written.
func TestEncryptedFinishedBeforeRecord(t *testing.T) {
	src, tape, big := encSetup(t)
	id := ageIdentity(t)
	o := encOpts(t, src, tape, id)
	crashAt(t, "manifest", int64(len(big)))
	if _, err := Put(o); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	testHook = nil
	sum, err := Put(o)
	if err != nil || sum.Resumed == 0 {
		t.Fatalf("%+v %v", sum, err)
	}
	entries, _ := loadEntries(tape)
	for _, e := range entries {
		if e.Path == "downloads/big.bin" && (e.Age == nil || e.SHA256 != sha(string(big))) {
			t.Fatalf("%+v", e)
		}
	}
	dest := t.TempDir()
	if res, err := Restore(RestoreOptions{TapeRoot: tape, Dest: dest, Log: io.Discard, Identities: []age.Identity{id}}); err != nil || res.Failed != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestEncryptedParityRepair(t *testing.T) {
	src, tape, big := encSetup(t)
	id := ageIdentity(t)
	o := encOpts(t, src, tape, id)
	o.Parity = 10
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	damage(t, filepath.Join(tape, "downloads", "big.bin.age"), 50<<10, 1000)
	if res := verify(t, tape); res.Repairable != 1 {
		t.Fatalf("%+v", res)
	}
	dest := t.TempDir()
	res, err := Restore(RestoreOptions{TapeRoot: tape, Dest: dest, Log: io.Discard, Identities: []age.Identity{id}})
	if err != nil || res.Repaired != 1 || res.Failed != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "downloads", "big.bin")); !bytes.Equal(got, big) {
		t.Fatal("repaired file differs")
	}
}

func TestEncryptedNameCollision(t *testing.T) {
	src, tape := setup(t)
	writeFile(t, filepath.Join(src, "x.age"), "a file that happens to end in .age")
	writeFile(t, filepath.Join(src, "x"), "x")
	id := ageIdentity(t)
	o := opts(t, src, tape)

	// An unencrypted x.age on tape, then x encrypted into the same place.
	o.Source, o.Prefix = filepath.Join(src, "x.age"), "downloads/x.age"
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	o.Source, o.Prefix = filepath.Join(src, "x"), "downloads/x"
	o.Recipients = []age.Recipient{id.Recipient()}
	if _, err := Put(o); err == nil || !strings.Contains(err.Error(), "already used by downloads/x.age") {
		t.Fatalf("%v", err)
	}

	// The other way round on a fresh tape.
	tape2 := newTape(t, tape, "tape2")
	o.TapeRoot, o.Again = tape2, true
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	o.Recipients = nil
	o.Source, o.Prefix = filepath.Join(src, "x.age"), "downloads/x.age"
	if _, err := Put(o); err == nil || !strings.Contains(err.Error(), "already used by downloads/x") {
		t.Fatalf("%v", err)
	}
	// Encrypted names never collide within one run.
	o.TapeRoot = newTape(t, tape, "tape3")
	o.Source, o.Prefix = src, ""
	o.Recipients = []age.Recipient{id.Recipient()}
	if sum, err := Put(o); err != nil || sum.Files != 5 {
		t.Fatalf("%+v %v", sum, err)
	}
}

// The journal of an encrypted file holds its key: owner only.
func TestEncryptedJournalIsPrivate(t *testing.T) {
	src, tape, _ := encSetup(t)
	o := encOpts(t, src, tape, ageIdentity(t))
	crashAt(t, "checkpoint", 64<<10)
	Put(o)
	names, _ := filepath.Glob(filepath.Join(o.Catalog.Dir, "journal", "*", "*.jsonl"))
	if len(names) == 0 {
		t.Fatal("no journal")
	}
	for _, n := range names {
		st, _ := os.Stat(n)
		b, _ := os.ReadFile(n)
		if st.Mode().Perm() != 0o600 || !strings.Contains(string(b), "file_key") {
			t.Fatalf("%s: mode %o", n, st.Mode().Perm())
		}
	}
	// Once recorded, the journal with the key is gone.
	testHook = nil
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	if names, _ := filepath.Glob(filepath.Join(o.Catalog.Dir, "journal", "*", "*.jsonl")); len(names) != 0 {
		t.Fatalf("journals left: %v", names)
	}
}

// Switching between encrypted and unencrypted runs never resumes the
// other kind of write.
func TestEncryptionChangeDoesNotResume(t *testing.T) {
	src, tape, big := encSetup(t)
	id := ageIdentity(t)
	o := encOpts(t, src, tape, id)
	plainOpts := o
	plainOpts.Recipients = nil
	crashAt(t, "checkpoint", 64<<10)
	Put(plainOpts)
	testHook = nil
	sum, err := Put(o)
	if err != nil || sum.Resumed != 0 {
		t.Fatalf("%+v %v", sum, err)
	}
	if got := decryptFile(t, filepath.Join(tape, "downloads", "big.bin.age"), id); !bytes.Equal(got, big) {
		t.Fatal("wrong content")
	}
}

func TestEncryptedCopiesPurgeAndRecover(t *testing.T) {
	src, tape, _ := encSetup(t)
	id := ageIdentity(t)
	o := encOpts(t, src, tape, id)
	if _, err := Put(o); err != nil {
		t.Fatal(err)
	}
	// Recover finds nothing unknown: the .age files belong to entries.
	rres, err := Recover(RecoverOptions{TapeRoot: tape, Catalog: o.Catalog, Log: io.Discard})
	if err != nil || rres.Files != 0 {
		t.Fatalf("recover: %+v %v", rres, err)
	}
	// Copies are counted by the content, encrypted or not.
	o2 := o
	o2.TapeRoot = newTape(t, tape, "tape2")
	if sum, err := Put(o2); err != nil || sum.Elsewhere != 4 {
		t.Fatalf("%+v %v", sum, err)
	}
	// Purge with --rehash compares the plaintext.
	verifyWith(t, tape, o.Catalog)
	p, err := PlanPurge(PurgeOptions{Source: src, Catalog: o.Catalog, Rehash: true})
	if err != nil || len(p.Delete) != 4 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestEncryptedPendingRecordRolledBack(t *testing.T) {
	src, tape, _ := encSetup(t)
	o := encOpts(t, src, tape, ageIdentity(t))
	o.FlushEvery = 1 << 40
	crashAt(t, "flush", 0)
	if _, err := Put(o); err == nil {
		t.Fatal("no crash")
	}
	testHook = nil
	// LTFS rolled back: the encrypted file is gone from tape.
	os.Remove(filepath.Join(tape, "downloads", "big.bin.age"))
	var log strings.Builder
	o.Log = &log
	sum, err := Put(o)
	if err != nil || !strings.Contains(log.String(), "no longer complete on tape") || sum.Files != 1 {
		t.Fatalf("%+v %v\n%s", sum, err, log.String())
	}
	if res := verify(t, tape); res.Verified != 4 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestEncryptedEntryValidation(t *testing.T) {
	good := manifest.Entry{Path: "a", Size: 5, SHA256: sha("alpha"), MTime: time.Now(),
		Age: &manifest.Age{Size: 5 + 16 + 200, SHA256: sha("x"), Recipients: []string{"age1abc"}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(e *manifest.Entry)) manifest.Entry {
		e := good
		a := *good.Age
		e.Age = &a
		f(&e)
		return e
	}
	bad := map[string]manifest.Entry{
		"too small":     mutate(func(e *manifest.Entry) { e.Age.Size = 21 }),
		"too large":     mutate(func(e *manifest.Entry) { e.Age.Size = 1 << 40 }),
		"bad hash":      mutate(func(e *manifest.Entry) { e.Age.SHA256 = "x" }),
		"no recipients": mutate(func(e *manifest.Entry) { e.Age.Recipients = nil }),
		"space":         mutate(func(e *manifest.Entry) { e.Age.Recipients = []string{"age1 x"} }),
		"newline":       mutate(func(e *manifest.Entry) { e.Age.Recipients = []string{"age1\nx"} }),
		"reference": mutate(func(e *manifest.Entry) {
			e.Ref = &manifest.Ref{Tape: "00000000-0000-4000-8000-000000000000", Path: "b"}
		}),
		"many recipients": mutate(func(e *manifest.Entry) { e.Age.Recipients = make([]string, 65) }),
		"recovered":       mutate(func(e *manifest.Entry) { e.Recovered = true }),
	}
	for name, e := range bad {
		if err := e.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if s := good.Stored(); s.Path != "a.age" || s.Size != good.Age.Size || s.SHA256 != good.Age.SHA256 || s.Age != nil {
		t.Fatalf("%+v", s)
	}
	plain := good
	plain.Age = nil
	if plain.Stored().Path != "a" || plain.TapePath() != "a" {
		t.Fatal("plain entry changed")
	}
}

// Resume with parity: checkpoints on parity window boundaries.
func TestEncryptedResumeWithParity(t *testing.T) {
	src, tape, big := encSetup(t)
	id := ageIdentity(t)
	o := encOpts(t, src, tape, id)
	o.Parity, o.ChunkSize, o.CheckpointEvery = 10, 256, 1
	crashAt(t, "checkpoint", 100<<10)
	if _, err := Put(o); !errors.Is(err, errCrash) {
		t.Fatalf("%v", err)
	}
	testHook = nil
	sum, err := Put(o)
	if err != nil || sum.Resumed == 0 {
		t.Fatalf("%+v %v", sum, err)
	}
	if got := decryptFile(t, filepath.Join(tape, "downloads", "big.bin.age"), id); !bytes.Equal(got, big) {
		t.Fatal("resumed file decrypts to different data")
	}
	damage(t, filepath.Join(tape, "downloads", "big.bin.age"), 120<<10, 300)
	dest := t.TempDir()
	res, err := Restore(RestoreOptions{TapeRoot: tape, Dest: dest, Log: io.Discard, Identities: []age.Identity{id}})
	if err != nil || res.Repaired != 1 || res.Failed != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "downloads", "big.bin")); !bytes.Equal(got, big) {
		t.Fatal("repaired file differs")
	}
}
