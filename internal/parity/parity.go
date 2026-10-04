// Package parity adds Reed-Solomon parity to archived files so that damaged
// regions of a tape can be rebuilt.
//
// LTO drives already correct random bit errors. What they cannot correct is
// a damaged area of tape (a crease, a scratch, stretched media), which shows
// up as a few megabytes that can no longer be read. Parity here targets
// exactly that: lost pieces whose position is known, because every piece has
// its own SHA-256. Reed-Solomon rebuilds one known-missing piece per parity
// piece.
//
// Two layouts are used:
//
// Large files (at least K chunks) use the archive chunks as data shards.
// Chunks are grouped into windows of K*D chunks. Within a window, chunk i
// belongs to stripe i mod D, so consecutive chunks land in different
// stripes and one contiguous damaged area of up to M*D chunks per window is
// recoverable. Parity is computed while streaming with D*M buffers.
//
// Small files are split into K equal shards, so the overhead stays at M/K
// even for tiny files. The shard hashes are stored with the parity.
package parity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/reedsolomon"
)

const (
	// K is the number of data shards per stripe.
	K = 20
	// D is the interleave depth: stripes per window for large files.
	D = 16
	// MaxPercent caps the parity overhead, which also caps memory use at
	// D*M chunks.
	MaxPercent = 25
	// MaxShardSize bounds shards, so repairing a stripe from a damaged or
	// crafted tape needs at most (maxK+maxM)*MaxShardSize of memory.
	MaxShardSize = 16 << 20

	maxK = 32
	maxM = 8
	maxD = 64
	// maxSize bounds file sizes so index arithmetic cannot overflow.
	maxSize = 1 << 50
)

// Shards returns the number of parity shards per stripe for an overhead
// given in percent, or 0 for no parity.
func Shards(percent int) (int, error) {
	if percent == 0 {
		return 0, nil
	}
	if percent < 0 || percent > MaxPercent {
		return 0, fmt.Errorf("parity must be between 0 and %d percent", MaxPercent)
	}
	return (K*percent + 99) / 100, nil
}

// Layout describes how the parity of one file is organized. It is stored on
// tape with the parity data.
type Layout struct {
	Scheme    string `json:"scheme"` // SchemeWindow or SchemeSmall
	K         int    `json:"k"`
	M         int    `json:"m"`
	D         int    `json:"d,omitempty"`
	ShardSize int64  `json:"shard_size"` // chunk size for window, shard size for small
	Size      int64  `json:"size"`       // file size
}

const (
	SchemeWindow = "rs-window"
	SchemeSmall  = "rs-small"
)

// ForFile returns the layout for a file of the given size, or nil if the
// file gets no parity.
func ForFile(size, chunkSize int64, m int) *Layout {
	if m == 0 || size == 0 {
		return nil
	}
	if size >= K*chunkSize {
		return &Layout{Scheme: SchemeWindow, K: K, M: m, D: D, ShardSize: chunkSize, Size: size}
	}
	return &Layout{Scheme: SchemeSmall, K: K, M: m, ShardSize: (size + K - 1) / K, Size: size}
}

// Validate checks a layout read from untrusted input.
func (l *Layout) Validate() error {
	switch {
	case l.Scheme != SchemeWindow && l.Scheme != SchemeSmall:
		return fmt.Errorf("unknown parity scheme %q", l.Scheme)
	case l.K < 1 || l.K > maxK || l.M < 1 || l.M > maxM:
		return errors.New("parity shard counts out of range")
	case l.Size < 1 || l.Size > maxSize || l.ShardSize < 1 || l.ShardSize > MaxShardSize:
		return errors.New("parity sizes out of range")
	case l.Scheme == SchemeWindow && (l.D < 1 || l.D > maxD || l.Size < int64(l.K)*l.ShardSize):
		return errors.New("window parity parameters out of range")
	case l.Scheme == SchemeSmall && (l.D != 0 || (l.Size+int64(l.K)-1)/int64(l.K) != l.ShardSize):
		return errors.New("small file parity parameters out of range")
	}
	return nil
}

// Chunks returns the number of data shards (chunks) of a window file.
func (l *Layout) Chunks() int64 {
	return (l.Size + l.ShardSize - 1) / l.ShardSize
}

// window describes window w of a window-layout file.
type window struct {
	first   int64 // first chunk index
	count   int64 // chunks in this window
	stripes int   // stripes in this window, at most D
}

func (l *Layout) window(w int64) window {
	per := int64(l.K * l.D)
	first := w * per
	count := min(per, l.Chunks()-first)
	stripes := int((count + int64(l.K) - 1) / int64(l.K))
	return window{first: first, count: count, stripes: min(stripes, l.D)}
}

// locate returns the window, stripe and position within the stripe of
// chunk i.
func (l *Layout) locate(i int64) (w int64, stripe, pos int) {
	per := int64(l.K * l.D)
	w = i / per
	win := l.window(w)
	off := i - win.first
	return w, int(off % int64(win.stripes)), int(off / int64(win.stripes))
}

// ParityShards returns the total number of parity shards for the file.
func (l *Layout) ParityShards() int64 {
	if l.Scheme == SchemeSmall {
		return int64(l.M)
	}
	per := int64(l.K * l.D)
	last := (l.Chunks() - 1) / per
	return l.parityIndex(last, 0, 0) + int64(l.window(last).stripes*l.M)
}

// ParitySize returns the number of parity bytes for the file.
func (l *Layout) ParitySize() int64 {
	return l.ParityShards() * l.ShardSize
}

// parityIndex returns the index of parity shard j of stripe s in window w,
// counting from the start of the file's parity data.
// Every window before the last one is full and has D stripes.
func (l *Layout) parityIndex(w int64, s, j int) int64 {
	if l.Scheme == SchemeSmall {
		return int64(j)
	}
	return w*int64(l.D*l.M) + int64(s*l.M+j)
}

func newCoder(k, m int) (reedsolomon.Encoder, error) {
	return reedsolomon.New(k, m)
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// WindowEncoder computes window parity while a large file streams by.
type WindowEncoder struct {
	l      *Layout
	out    io.Writer
	coders map[int]reedsolomon.Encoder // by data shards per stripe
	parity [][][]byte                  // [stripe][j]
	pad    []byte
	next   int64 // next expected chunk index
	hashes []string
}

// NewWindowEncoder starts encoding at chunk start, which must be the first
// chunk of a window. Completed windows are written to out.
func NewWindowEncoder(l *Layout, start int64, out io.Writer) (*WindowEncoder, error) {
	if l.Scheme != SchemeWindow {
		return nil, errors.New("not a window layout")
	}
	if start%int64(l.K*l.D) != 0 {
		return nil, errors.New("parity must start at a window boundary")
	}
	e := &WindowEncoder{l: l, out: out, coders: map[int]reedsolomon.Encoder{}, next: start, pad: make([]byte, l.ShardSize)}
	e.parity = make([][][]byte, l.D)
	for s := range e.parity {
		e.parity[s] = make([][]byte, l.M)
		for j := range e.parity[s] {
			e.parity[s][j] = make([]byte, l.ShardSize)
		}
	}
	return e, nil
}

// dataShards returns the number of data shards of stripe s in window win.
func dataShards(win window, s int) int {
	n := win.count / int64(win.stripes)
	if int64(s) < win.count%int64(win.stripes) {
		n++
	}
	return int(n)
}

func (e *WindowEncoder) coder(k int) (reedsolomon.Encoder, error) {
	if c, ok := e.coders[k]; ok {
		return c, nil
	}
	c, err := newCoder(k, e.l.M)
	if err != nil {
		return nil, err
	}
	e.coders[k] = c
	return c, nil
}

// Add feeds the next chunk. Chunks must arrive in order.
func (e *WindowEncoder) Add(chunk []byte) error {
	i := e.next
	if i >= e.l.Chunks() {
		return errors.New("more chunks than the layout allows")
	}
	if int64(len(chunk)) > e.l.ShardSize {
		return errors.New("chunk larger than the shard size")
	}
	w, s, pos := e.l.locate(i)
	win := e.l.window(w)
	c, err := e.coder(dataShards(win, s))
	if err != nil {
		return err
	}
	shard := chunk
	if int64(len(chunk)) < e.l.ShardSize {
		shard = append(append(e.pad[:0:0], chunk...), e.pad[len(chunk):]...)
	}
	if err := c.EncodeIdx(shard, pos, e.parity[s]); err != nil {
		return err
	}
	e.next++
	if e.next == win.first+win.count {
		return e.emit(win)
	}
	return nil
}

func (e *WindowEncoder) emit(win window) error {
	for s := range win.stripes {
		for j := range e.l.M {
			p := e.parity[s][j]
			if _, err := e.out.Write(p); err != nil {
				return err
			}
			e.hashes = append(e.hashes, hashHex(p))
			clear(p)
		}
	}
	return nil
}

// Next returns the index of the next chunk expected.
func (e *WindowEncoder) Next() int64 { return e.next }

// AtWindowBoundary reports whether all fed chunks form complete windows,
// so no parity state is held in memory.
func (e *WindowEncoder) AtWindowBoundary() bool {
	return e.next%int64(e.l.K*e.l.D) == 0 || e.next == e.l.Chunks()
}

// Hashes returns the SHA-256 of every parity shard emitted so far.
func (e *WindowEncoder) Hashes() []string { return e.hashes }

// SetHashes restores the parity shard hashes of windows emitted before a
// resume.
func (e *WindowEncoder) SetHashes(h []string) { e.hashes = append([]string(nil), h...) }

// EncodeSmall computes parity for a small file held in memory. It returns
// the parity bytes, the data shard hashes and the parity shard hashes.
func EncodeSmall(l *Layout, data []byte) (parity []byte, dataHashes, parityHashes []string, err error) {
	if l.Scheme != SchemeSmall || int64(len(data)) != l.Size {
		return nil, nil, nil, errors.New("layout does not match data")
	}
	shards := splitSmall(l, data)
	c, err := newCoder(l.K, l.M)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := c.Encode(shards); err != nil {
		return nil, nil, nil, err
	}
	for i, s := range shards {
		if i < l.K {
			dataHashes = append(dataHashes, hashHex(s))
		} else {
			parity = append(parity, s...)
			parityHashes = append(parityHashes, hashHex(s))
		}
	}
	return parity, dataHashes, parityHashes, nil
}

// splitSmall returns K zero-padded data shards plus M empty parity shards.
func splitSmall(l *Layout, data []byte) [][]byte {
	shards := make([][]byte, l.K+l.M)
	for i := range shards {
		shards[i] = make([]byte, l.ShardSize)
		if i < l.K {
			lo := int64(i) * l.ShardSize
			if lo < int64(len(data)) {
				copy(shards[i], data[lo:min(lo+l.ShardSize, int64(len(data)))])
			}
		}
	}
	return shards
}
