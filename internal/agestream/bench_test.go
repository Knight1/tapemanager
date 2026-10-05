package agestream

import (
	"bytes"
	"io"
	"testing"
)

func BenchmarkEncrypter(b *testing.B) {
	id := newIdentity(b)
	p, _ := NewParams(id.Recipient())
	plain := randomBytes(b, 64<<20)
	b.SetBytes(int64(len(plain)))
	for b.Loop() {
		e, _ := NewEncrypter(p, bytes.NewReader(plain), int64(len(plain)), 4<<20)
		io.Copy(io.Discard, struct{ io.Reader }{e})
	}
}
