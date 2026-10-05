package agestream

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"filippo.io/age"
)

func randomBytes(t testing.TB, n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func newIdentity(t testing.TB) *age.X25519Identity {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// encryptAll reads the whole encrypted file.
func encryptAll(t testing.TB, p *Params, plain []byte, every int64) ([]byte, *Encrypter) {
	t.Helper()
	e, err := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), every)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(e)
	if err != nil {
		t.Fatal(err)
	}
	return out, e
}

func decrypt(t testing.TB, data []byte, ids ...age.Identity) []byte {
	t.Helper()
	r, err := age.Decrypt(bytes.NewReader(data), ids...)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestRoundTripWithAge(t *testing.T) {
	id := newIdentity(t)
	for _, n := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3 * ChunkSize, 3*ChunkSize + 17} {
		plain := randomBytes(t, n)
		p, err := NewParams(id.Recipient())
		if err != nil {
			t.Fatal(err)
		}
		out, e := encryptAll(t, p, plain, 4096)
		if int64(len(out)) != p.StoredSize(int64(n)) || e.Size() != int64(len(out)) {
			t.Fatalf("%d: stored %d, predicted %d", n, len(out), p.StoredSize(int64(n)))
		}
		if got := decrypt(t, out, id); !bytes.Equal(got, plain) {
			t.Fatalf("%d: decrypted data differs", n)
		}
		if s, err := e.PlainSHA256(); err != nil || s != sum(plain) {
			t.Fatalf("%d: plain hash %s %v", n, s, err)
		}
		// Random access decryption, as restore uses it.
		ra, size, err := age.DecryptReaderAt(bytes.NewReader(out), int64(len(out)), id)
		if err != nil || size != int64(n) {
			t.Fatalf("%d: %v %d", n, err, size)
		}
		got := make([]byte, n)
		if _, err := ra.ReadAt(got, 0); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("%d: random access differs", n)
		}
	}
}

func TestSeveralAndPostQuantumRecipients(t *testing.T) {
	a, b := newIdentity(t), newIdentity(t)
	plain := randomBytes(t, 2*ChunkSize+5)
	p, err := NewParams(a.Recipient(), b.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	out, _ := encryptAll(t, p, plain, 1<<20)
	for _, id := range []age.Identity{a, b} {
		if !bytes.Equal(decrypt(t, out, id), plain) {
			t.Fatal("recipient cannot decrypt")
		}
	}
	if _, err := age.Decrypt(bytes.NewReader(out), newIdentity(t)); err == nil {
		t.Fatal("stranger decrypted")
	}

	pq, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	p, err = NewParams(pq.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	out, _ = encryptAll(t, p, plain, 1<<20)
	if !bytes.Equal(decrypt(t, out, pq), plain) {
		t.Fatal("post-quantum recipient cannot decrypt")
	}
	// age refuses to mix post-quantum and classic recipients.
	if _, err := NewParams(pq.Recipient(), a.Recipient()); err == nil {
		t.Fatal("mixed recipients accepted")
	}
	if _, err := NewParams(); err == nil {
		t.Fatal("no recipients accepted")
	}
}

// Every checkpoint position resumes into the exact same file.
func TestResumeAtEveryCheckpoint(t *testing.T) {
	id := newIdentity(t)
	plain := randomBytes(t, 5*ChunkSize+333)
	p, _ := NewParams(id.Recipient())
	const every = 40000 // not aligned with chunks on purpose
	full, _ := encryptAll(t, p, plain, every)

	for off := int64(every); off <= int64(len(full)); off += every {
		// First run: read past the checkpoint, as the read-ahead does.
		e1, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), every)
		buf := make([]byte, min(off+3*every, int64(len(full))))
		if _, err := io.ReadFull(e1, buf); err != nil {
			t.Fatal(err)
		}
		state, next, err := e1.Snapshot(off)
		if err != nil {
			t.Fatalf("snapshot at %d: %v", off, err)
		}
		// Second run continues from what is on tape.
		e2, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), every)
		if err := e2.ResumeAt(off, state, next, bytes.NewReader(full[:off])); err != nil {
			t.Fatalf("resume at %d: %v", off, err)
		}
		rest, err := io.ReadAll(e2)
		if err != nil {
			t.Fatal(err)
		}
		if got := append(bytes.Clone(full[:off]), rest...); !bytes.Equal(got, full) {
			t.Fatalf("resume at %d produced a different file", off)
		}
		if s, err := e2.PlainSHA256(); err != nil || s != sum(plain) {
			t.Fatalf("resume at %d: plain hash %v", off, err)
		}
	}
}

func TestResumeInHeader(t *testing.T) {
	id := newIdentity(t)
	plain := randomBytes(t, 1000)
	p, _ := NewParams(id.Recipient())
	full, _ := encryptAll(t, p, plain, 16)
	for _, off := range []int64{16, 32, p.PayloadStart()} {
		e1, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), 16)
		io.ReadFull(e1, make([]byte, off))
		state, next, err := e1.Snapshot(off)
		if err != nil {
			t.Fatal(err)
		}
		e2, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), 16)
		if err := e2.ResumeAt(off, state, next, bytes.NewReader(full[:off])); err != nil {
			t.Fatal(err)
		}
		rest, _ := io.ReadAll(e2)
		if !bytes.Equal(append(bytes.Clone(full[:off]), rest...), full) {
			t.Fatalf("resume at %d differs", off)
		}
	}
}

func TestResumeDetectsChangedSource(t *testing.T) {
	id := newIdentity(t)
	plain := randomBytes(t, 3*ChunkSize)
	p, _ := NewParams(id.Recipient())
	const every = 100000
	full, _ := encryptAll(t, p, plain, every)
	e1, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), every)
	io.ReadFull(e1, make([]byte, every+10))
	state, next, _ := e1.Snapshot(every)

	// The source changed inside the chunk that is partly on tape.
	changed := bytes.Clone(plain)
	changed[next*ChunkSize] ^= 1
	e2, _ := NewEncrypter(p, bytes.NewReader(changed), int64(len(changed)), every)
	if err := e2.ResumeAt(every, state, next, bytes.NewReader(full[:every])); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed source: %v", err)
	}
	// A shrunk source is detected while reading.
	e3, _ := NewEncrypter(p, bytes.NewReader(plain[:100]), int64(len(plain)), every)
	if _, err := io.ReadAll(e3); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("short source: %v", err)
	}
	// Inconsistent checkpoints are refused.
	e4, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), every)
	if err := e4.ResumeAt(every, state, next+1, bytes.NewReader(full)); err == nil {
		t.Fatal("wrong chunk accepted")
	}
	if err := e4.ResumeAt(every, []byte("junk"), next, bytes.NewReader(full)); err == nil {
		t.Fatal("bad hash state accepted")
	}
	if err := e4.ResumeAt(int64(len(full))+1, state, next, bytes.NewReader(full)); err == nil {
		t.Fatal("offset past the end accepted")
	}
}

func TestParamsValidate(t *testing.T) {
	id := newIdentity(t)
	p, _ := NewParams(id.Recipient())
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(q *Params)) *Params {
		q := &Params{Header: bytes.Clone(p.Header), Nonce: bytes.Clone(p.Nonce), FileKey: bytes.Clone(p.FileKey)}
		f(q)
		return q
	}
	bad := map[string]*Params{
		"header byte": mutate(func(q *Params) { q.Header[30] ^= 1 }),
		"file key":    mutate(func(q *Params) { q.FileKey[0] ^= 1 }),
		"key length":  mutate(func(q *Params) { q.FileKey = q.FileKey[:8] }),
		"nonce":       mutate(func(q *Params) { q.Nonce = nil }),
		"magic":       mutate(func(q *Params) { q.Header = append([]byte("x"), q.Header...) }),
		"no MAC":      mutate(func(q *Params) { q.Header = []byte(magic) }),
		"huge":        mutate(func(q *Params) { q.Header = make([]byte, MaxHeader+1) }),
		"bad base64":  mutate(func(q *Params) { q.Header = append(q.Header[:len(q.Header)-3], '!', '!', '\n') }),
	}
	for name, q := range bad {
		if err := q.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
		if _, err := NewEncrypter(q, bytes.NewReader(nil), 0, 1); err == nil {
			t.Errorf("%s: encrypter created", name)
		}
	}
	if _, err := NewEncrypter(p, bytes.NewReader(nil), -1, 1); err == nil {
		t.Error("negative size accepted")
	}
	if _, err := NewEncrypter(p, bytes.NewReader(nil), 0, 0); err == nil {
		t.Error("zero spacing accepted")
	}
}

// Each file gets a new key and nonce.
func TestParamsAreUnique(t *testing.T) {
	r := newIdentity(t).Recipient()
	a, _ := NewParams(r)
	b, _ := NewParams(r)
	if bytes.Equal(a.FileKey, b.FileKey) || bytes.Equal(a.Nonce, b.Nonce) {
		t.Fatal("key or nonce reused")
	}
}

func TestSnapshotErrors(t *testing.T) {
	id := newIdentity(t)
	plain := randomBytes(t, 4*ChunkSize)
	p, _ := NewParams(id.Recipient())
	e, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), 1<<30)
	io.ReadAll(e)
	// Spacing larger than the file: only the start and the end exist.
	if _, _, err := e.Snapshot(int64(2*ChunkSize) + p.PayloadStart()); err == nil {
		t.Fatal("snapshot at a position that is no checkpoint")
	}
	if _, next, err := e.Snapshot(e.Size()); err != nil || next != Chunks(int64(len(plain))) {
		t.Fatalf("end: %d %v", next, err)
	}
	if _, err := e.PlainSHA256(); err != nil {
		t.Fatal(err)
	}
	e2, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), 1<<30)
	if _, err := e2.PlainSHA256(); err == nil {
		t.Fatal("hash before reading")
	}
}
