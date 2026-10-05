package agestream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"sync"
	"testing"

	"filippo.io/age"
)

var (
	fuzzOnce sync.Once
	fuzzID   *age.X25519Identity
	fuzzP    *Params
)

func fuzzParams(t testing.TB) (*age.X25519Identity, *Params) {
	fuzzOnce.Do(func() {
		fuzzID, _ = age.GenerateX25519Identity()
		fuzzP, _ = NewParams(fuzzID.Recipient())
	})
	return fuzzID, fuzzP
}

// readRandom reads all of r in pieces of random size, as different
// callers might.
func readRandom(t *testing.T, r io.Reader, rng *rand.Rand, limit int64) []byte {
	var out []byte
	for limit < 0 || int64(len(out)) < limit {
		n := rng.IntN(3*ChunkSize) + 1
		if limit >= 0 {
			n = min(n, int(limit-int64(len(out))))
		}
		buf := make([]byte, n)
		m, err := r.Read(buf)
		out = append(out, buf[:m]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// Any plaintext, checkpoint spacing, read pattern and resume point must
// produce one file that the age library decrypts to the plaintext.
func FuzzEncryptResume(f *testing.F) {
	f.Add(uint64(1), uint32(5*ChunkSize+333), uint32(40000), uint16(3))
	f.Add(uint64(2), uint32(0), uint32(1), uint16(0))
	f.Add(uint64(3), uint32(ChunkSize), uint32(ChunkSize+16), uint16(1))
	f.Fuzz(func(t *testing.T, seed uint64, size, every uint32, k uint16) {
		id, p := fuzzParams(t)
		rng := rand.New(rand.NewPCG(seed, 3))
		plain := make([]byte, size%(6*ChunkSize))
		for i := range plain {
			plain[i] = byte(rng.Uint32())
		}
		ev := int64(every%(4*sealedSize)) + 1
		e1, err := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), ev)
		if err != nil {
			t.Fatal(err)
		}
		full := readRandom(t, e1, rng, -1)
		if int64(len(full)) != e1.Size() || e1.Size() != p.StoredSize(int64(len(plain))) {
			t.Fatalf("size %d, Size() %d", len(full), e1.Size())
		}
		r, err := age.Decrypt(bytes.NewReader(full), id)
		if err != nil {
			t.Fatalf("age cannot decrypt: %v", err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("age decrypted %d bytes, err %v", len(got), err)
		}
		want := sha256.Sum256(plain)
		if s, err := e1.PlainSHA256(); err != nil || s != hex.EncodeToString(want[:]) {
			t.Fatalf("plain hash %v", err)
		}

		// Interrupt at checkpoint k, with read-ahead past it, and resume.
		nk := e1.Size() / ev
		if nk == 0 {
			return
		}
		off := (int64(k)%nk + 1) * ev
		e2, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), ev)
		readRandom(t, e2, rng, min(off+int64(rng.IntN(4*ChunkSize)), e2.Size()))
		state, next, err := e2.Snapshot(off)
		if err != nil {
			t.Fatalf("snapshot at %d of %d (every %d): %v", off, e2.Size(), ev, err)
		}
		e3, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), ev)
		if err := e3.ResumeAt(off, state, next, bytes.NewReader(full[:off])); err != nil {
			t.Fatalf("resume at %d: %v", off, err)
		}
		rest := readRandom(t, e3, rng, -1)
		if !bytes.Equal(append(bytes.Clone(full[:off]), rest...), full) {
			t.Fatalf("resume at %d (every %d) produced a different file", off, ev)
		}
		if s, err := e3.PlainSHA256(); err != nil || s != hex.EncodeToString(want[:]) {
			t.Fatalf("resume at %d: plain hash %v", off, err)
		}
	})
}

// Params come from a journal on disk. Damaged ones must be refused, never
// accepted with a header that does not match the key.
func FuzzParamsValidate(f *testing.F) {
	_, p := fuzzParams(f)
	f.Add(p.Header, p.Nonce, p.FileKey)
	f.Fuzz(func(t *testing.T, header, nonce, key []byte) {
		_, good := fuzzParams(t)
		q := &Params{Header: header, Nonce: nonce, FileKey: key}
		if q.Validate() != nil {
			return
		}
		if bytes.Equal(key, good.FileKey) && !bytes.Equal(header, good.Header) && bytes.HasPrefix(header, good.Header[:len(good.Header)-45]) {
			t.Fatalf("modified header accepted")
		}
		if _, err := NewEncrypter(q, bytes.NewReader(nil), 0, 1); err != nil {
			t.Fatalf("validated params rejected: %v", err)
		}
	})
}
