package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
)

var (
	crashIDOnce sync.Once
	crashID     *age.X25519Identity
)

// FuzzPutCrash interrupts a put at a random hook call, as a crash would:
// after the crash point nothing else is written to the tape. The next runs must archive everything, and every file must verify and
// restore to its original bytes.
func FuzzPutCrash(f *testing.F) {
	f.Add(uint64(1), uint8(3), uint16(5), uint8(0))
	f.Add(uint64(2), uint8(6), uint16(17), uint8(1))
	f.Add(uint64(3), uint8(4), uint16(2), uint8(2))
	f.Add(uint64(4), uint8(5), uint16(9), uint8(3))
	f.Fuzz(func(t *testing.T, seed uint64, nfiles uint8, crashAt uint16, mode uint8) {
		rng := rand.New(rand.NewPCG(seed, 11))
		base := t.TempDir()
		src, tape := filepath.Join(base, "src"), filepath.Join(base, "tape")
		os.MkdirAll(tape, 0o755)
		files := map[string][]byte{}
		for i := range int(nfiles%6) + 1 {
			data := make([]byte, rng.IntN(3000))
			for j := range data {
				data[j] = byte(rng.Uint32())
			}
			name := fmt.Sprintf("d%d/f%d", i%2, i)
			files[name] = data
			writeFile(t, filepath.Join(src, name), string(data))
		}
		o := opts(t, src, tape)
		o.ChunkSize = 64
		o.CheckpointEvery = 128
		o.FlushEvery = int64(rng.IntN(4000)) + 1
		if mode&1 != 0 {
			o.Parity = 10
		}
		if mode&2 != 0 {
			crashIDOnce.Do(func() { crashID, _ = age.GenerateX25519Identity() })
			o.Recipients = []age.Recipient{crashID.Recipient()}
		}

		calls, crashed := 0, false
		testHook = func(stage string, off int64) error {
			calls++
			if crashed || calls > int(crashAt%64) {
				crashed = true
				return errCrash
			}
			return nil
		}
		_, err := Put(o)
		testHook = nil
		if err != nil && !errors.Is(err, errCrash) {
			t.Fatalf("first run: %v", err)
		}

		for run := 0; ; run++ {
			_, err := Put(o)
			if err == nil {
				break
			}
			if run == 2 {
				t.Fatalf("put still fails after a crash (calls %d): %v", calls, err)
			}
		}

		res, err := Verify(VerifyOptions{TapeRoot: tape, Log: io.Discard})
		if err != nil || res.Verified != len(files) || res.Failed+res.Repairable+res.Problems != 0 {
			t.Fatalf("verify after crash at call %d: %+v %v", calls, res, err)
		}
		filepath.WalkDir(tape, func(p string, d fs.DirEntry, err error) error {
			if err == nil && strings.HasSuffix(p, PartialSuffix) {
				t.Fatalf("partial file left behind: %s", p)
			}
			return nil
		})
		ro := RestoreOptions{TapeRoot: tape, Dest: filepath.Join(base, "out"), Log: io.Discard}
		if crashID != nil && mode&2 != 0 {
			ro.Identities = []age.Identity{crashID}
		}
		rres, err := Restore(ro)
		if err != nil || rres.Files != len(files) || rres.Failed != 0 {
			t.Fatalf("restore: %+v %v", rres, err)
		}
		for name, data := range files {
			got, err := os.ReadFile(filepath.Join(base, "out", "src", name))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%s restored wrong (%v)", name, err)
			}
		}
	})
}
