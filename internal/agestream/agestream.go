// Package agestream produces files in the age format (age-encryption.org/v1)
// in a way that can be resumed after an interruption.
//
// The header, including the wrapped file key and the header MAC, is written
// by the age library itself. The payload (STREAM: ChaCha20-Poly1305 over
// 64 KiB chunks, keyed from the file key and a 16-byte nonce) is a pure
// function of the file key, the nonce and the plaintext, so it is produced
// here and any part of it can be regenerated later. The result decrypts
// with the age library and the age command line tool.
package agestream

import (
	"bytes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"

	"filippo.io/age"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// ChunkSize is the plaintext size of a payload chunk.
	ChunkSize = 64 << 10
	tagSize   = chacha20poly1305.Overhead
	// sealedSize is the size of a full chunk on tape.
	sealedSize  = ChunkSize + tagSize
	nonceSize   = 16
	fileKeySize = 16
	// MaxHeader bounds the header so a damaged journal cannot force a
	// large allocation.
	MaxHeader = 1 << 20
	magic     = "age-encryption.org/v1\n"
)

// Params are what is needed to regenerate an encrypted file: the header
// written by the age library, the payload nonce and the file key. The file
// key is secret: whoever has it can decrypt the file.
type Params struct {
	Header  []byte `json:"header"`
	Nonce   []byte `json:"nonce"`
	FileKey []byte `json:"file_key"`
}

// capture wraps a recipient to learn the file key age chooses.
type capture struct {
	r   age.Recipient
	key *[]byte
}

func (c capture) note(fileKey []byte) error {
	if *c.key == nil {
		*c.key = bytes.Clone(fileKey)
	} else if !bytes.Equal(*c.key, fileKey) {
		return errors.New("age used different file keys for its recipients")
	}
	return nil
}

func (c capture) Wrap(fileKey []byte) ([]*age.Stanza, error) {
	if err := c.note(fileKey); err != nil {
		return nil, err
	}
	return c.r.Wrap(fileKey)
}

// WrapWithLabels keeps age's rules for recipients that must not be mixed,
// such as post-quantum ones.
func (c capture) WrapWithLabels(fileKey []byte) ([]*age.Stanza, []string, error) {
	if err := c.note(fileKey); err != nil {
		return nil, nil, err
	}
	if rl, ok := c.r.(age.RecipientWithLabels); ok {
		return rl.WrapWithLabels(fileKey)
	}
	s, err := c.r.Wrap(fileKey)
	return s, nil, err
}

// NewParams creates the header and keys for a new file. The age library
// generates the file key, wraps it for every recipient and writes the
// header and nonce; only the payload is produced by this package.
func NewParams(recipients ...age.Recipient) (*Params, error) {
	if len(recipients) == 0 {
		return nil, errors.New("no recipients")
	}
	var key []byte
	wrapped := make([]age.Recipient, len(recipients))
	for i, r := range recipients {
		wrapped[i] = capture{r: r, key: &key}
	}
	var buf bytes.Buffer
	// Encrypt writes the header and the nonce before returning. The
	// returned writer is never used or closed, so no payload is written.
	if _, err := age.Encrypt(&buf, wrapped...); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) <= nonceSize || len(key) != fileKeySize {
		return nil, errors.New("age did not produce a header")
	}
	p := &Params{Header: bytes.Clone(b[:len(b)-nonceSize]), Nonce: bytes.Clone(b[len(b)-nonceSize:]), FileKey: key}
	return p, p.Validate()
}

// Validate checks params loaded from a journal, including the header MAC,
// so a damaged journal cannot produce a file that does not decrypt.
func (p *Params) Validate() error {
	if len(p.Nonce) != nonceSize || len(p.FileKey) != fileKeySize {
		return errors.New("invalid nonce or file key")
	}
	if len(p.Header) < len(magic) || len(p.Header) > MaxHeader || !bytes.HasPrefix(p.Header, []byte(magic)) || p.Header[len(p.Header)-1] != '\n' {
		return errors.New("invalid age header")
	}
	// The header ends with "--- " and the base64 MAC over everything
	// before the space.
	i := bytes.LastIndex(p.Header, []byte("\n--- "))
	if i < 0 {
		return errors.New("age header has no MAC")
	}
	got, err := decodeB64(p.Header[i+5 : len(p.Header)-1])
	if err != nil {
		return errors.New("age header has an invalid MAC")
	}
	h := hkdf.New(sha256.New, p.FileKey, nil, []byte("header"))
	macKey := make([]byte, 32)
	if _, err := io.ReadFull(h, macKey); err != nil {
		return err
	}
	m := hmac.New(sha256.New, macKey)
	m.Write(p.Header[:i+4])
	if !hmac.Equal(m.Sum(nil), got) {
		return errors.New("age header MAC does not match the file key")
	}
	return nil
}

// PayloadStart is the offset of the first payload chunk.
func (p *Params) PayloadStart() int64 { return int64(len(p.Header)) + nonceSize }

// Chunks is the number of payload chunks for a plaintext size. An empty
// file has one empty chunk.
func Chunks(plainSize int64) int64 {
	if plainSize == 0 {
		return 1
	}
	return (plainSize + ChunkSize - 1) / ChunkSize
}

// StoredSize is the size of the encrypted file.
func (p *Params) StoredSize(plainSize int64) int64 {
	return p.PayloadStart() + plainSize + tagSize*Chunks(plainSize)
}

func (p *Params) aead() (cipher.AEAD, error) {
	h := hkdf.New(sha256.New, p.FileKey, p.Nonce, []byte("payload"))
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(h, k); err != nil {
		return nil, err
	}
	return chacha20poly1305.New(k)
}

// ErrSourceChanged means the plaintext no longer matches what was read
// before, so a resume would mix two versions under the same key and nonce.
var ErrSourceChanged = errors.New("source changed since the interrupted write")

// Encrypter reads the encrypted file, generated from the plaintext on the
// fly. It also hashes the plaintext, and keeps the hash state at every
// position where the caller may checkpoint, so a resume can continue the
// plaintext hash too.
type Encrypter struct {
	p         *Params
	aead      cipher.AEAD
	src       io.ReaderAt
	plainSize int64
	chunks    int64
	start     int64 // payload start
	size      int64 // stored size
	pos       int64

	cur    []byte // sealed chunk curIdx
	curIdx int64
	pt     []byte

	// Checkpoints fall on multiples of every; snaps holds the plaintext
	// hash state before the chunk containing each such position.
	every int64
	nextK int64

	mu       sync.Mutex
	h        hash.Hash
	nextHash int64 // next chunk to hash
	snaps    map[int64][]byte
}

// NewEncrypter returns a reader of the encrypted form of src. every is the
// spacing of possible checkpoints in the encrypted file.
func NewEncrypter(p *Params, src io.ReaderAt, plainSize, every int64) (*Encrypter, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if plainSize < 0 || every <= 0 {
		return nil, errors.New("invalid size")
	}
	a, err := p.aead()
	if err != nil {
		return nil, err
	}
	return &Encrypter{
		p: p, aead: a, src: src, plainSize: plainSize, chunks: Chunks(plainSize),
		start: p.PayloadStart(), size: p.StoredSize(plainSize), curIdx: -1,
		pt: make([]byte, ChunkSize), every: every, nextK: 1,
		h: sha256.New(), snaps: map[int64][]byte{},
	}, nil
}

// Size is the size of the encrypted file.
func (e *Encrypter) Size() int64 { return e.size }

// chunkAt is the chunk holding stored offset off; offsets in the header
// count as chunk 0.
func (e *Encrypter) chunkAt(off int64) int64 {
	if off <= e.start {
		return 0
	}
	return min((off-e.start)/sealedSize, e.chunks-1)
}

func (e *Encrypter) nonce(i int64) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(n[3:11], uint64(i))
	if i == e.chunks-1 {
		n[11] = 1
	}
	return n
}

// seal encrypts chunk i into e.cur, hashing its plaintext the first time.
func (e *Encrypter) seal(i int64) error {
	off := i * ChunkSize
	n := min(int64(ChunkSize), e.plainSize-off)
	pt := e.pt[:n]
	if m, err := e.src.ReadAt(pt, off); int64(m) != n {
		if err == nil || err == io.EOF {
			err = ErrSourceChanged
		}
		return err
	}
	// Hashing and encrypting both only read pt, so they run side by side.
	var wg sync.WaitGroup
	wg.Go(func() { e.cur = e.aead.Seal(e.cur[:0], e.nonce(i), pt, nil) })
	e.mu.Lock()
	var err error
	switch {
	case i == e.nextHash:
		for e.chunkAt(e.nextK*e.every) < i {
			e.nextK++
		}
		if e.chunkAt(e.nextK*e.every) == i {
			if st, serr := e.state(); serr == nil {
				e.snaps[i] = st
			}
		}
		e.h.Write(pt)
		e.nextHash++
	case i > e.nextHash:
		err = fmt.Errorf("chunk %d read before chunk %d", i, e.nextHash)
	}
	e.mu.Unlock()
	wg.Wait()
	if err != nil {
		e.curIdx = -1
		return err
	}
	e.curIdx = i
	return nil
}

func (e *Encrypter) state() ([]byte, error) {
	return e.h.(encoding.BinaryMarshaler).MarshalBinary()
}

// Read implements io.Reader.
func (e *Encrypter) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && e.pos < e.size {
		if e.pos < e.start {
			hdr := e.pos
			var c int
			if hdr < int64(len(e.p.Header)) {
				c = copy(p[n:], e.p.Header[hdr:])
			} else {
				c = copy(p[n:], e.p.Nonce[hdr-int64(len(e.p.Header)):])
			}
			n += c
			e.pos += int64(c)
			continue
		}
		i := (e.pos - e.start) / sealedSize
		if i != e.curIdx {
			if err := e.seal(i); err != nil {
				return n, err
			}
		}
		c := copy(p[n:], e.cur[e.pos-e.start-i*sealedSize:])
		n += c
		e.pos += int64(c)
	}
	if n == 0 && e.pos >= e.size {
		return 0, io.EOF
	}
	return n, nil
}

// Snapshot returns the plaintext hash state for a checkpoint at stored
// offset off and the chunk it is followed by. off must be a multiple of
// the checkpoint spacing (or the end), and the reader must be past it.
// Older snapshots are dropped.
func (e *Encrypter) Snapshot(off int64) (state []byte, next int64, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.chunkAt(off)
	if off >= e.size {
		c = e.chunks
	}
	if c == e.nextHash {
		state, err = e.state()
	} else if st, ok := e.snaps[c]; ok {
		state = st
	} else {
		err = fmt.Errorf("no plaintext hash state for offset %d", off)
	}
	for k := range e.snaps {
		if k < c {
			delete(e.snaps, k)
		}
	}
	return state, c, err
}

// ResumeAt continues at stored offset off from a checkpoint's plaintext
// hash state. written reads what is already on tape; the part of the
// current chunk before off is regenerated and compared with it, so a
// changed source is detected (ErrSourceChanged) before anything is written
// under the same key and nonce.
func (e *Encrypter) ResumeAt(off int64, state []byte, next int64, written io.ReaderAt) error {
	// A checkpoint at the very end follows the last chunk (see Snapshot).
	end := off == e.size
	expect := e.chunkAt(off)
	if end {
		expect = e.chunks
	}
	if off < 0 || off > e.size || next != expect {
		return errors.New("checkpoint does not match the encrypted file")
	}
	h := sha256.New()
	if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
		return fmt.Errorf("restoring plaintext hash: %w", err)
	}
	e.mu.Lock()
	e.h, e.nextHash, e.snaps = h, next, map[int64][]byte{}
	e.mu.Unlock()
	e.pos, e.curIdx = off, -1

	// Compare what is on tape before off with what would be written.
	var from int64
	var want []byte
	if off <= e.start {
		from, want = 0, append(bytes.Clone(e.p.Header), e.p.Nonce...)[:off]
	} else {
		// At the end, the last chunk is compared; it was hashed already.
		i := min(next, e.chunks-1)
		if err := e.seal(i); err != nil {
			return err
		}
		from = e.start + i*sealedSize
		want = e.cur[:off-from]
	}
	if len(want) == 0 {
		return nil
	}
	got := make([]byte, len(want))
	if _, err := written.ReadAt(got, from); err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return ErrSourceChanged
	}
	return nil
}

// PlainSHA256 returns the SHA-256 of the plaintext once all of it was read.
func (e *Encrypter) PlainSHA256() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.nextHash != e.chunks {
		return "", errors.New("plaintext not completely read")
	}
	return hex.EncodeToString(e.h.Sum(nil)), nil
}

func decodeB64(b []byte) ([]byte, error) {
	return base64.RawStdEncoding.Strict().DecodeString(string(b))
}
