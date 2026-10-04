package parity

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
)

const testChunk = 16

type memReader struct {
	data     []byte
	parity   []byte
	l        *Layout
	lost     map[int64]bool // unreadable chunks
	lostPar  map[int64]bool
	corrupt  map[int64]bool // chunks that read back wrong
	parReads int
}

func (r *memReader) Chunk(i int64) ([]byte, error) {
	if r.lost[i] {
		return nil, errors.New("EIO")
	}
	lo := i * r.l.ShardSize
	b := append([]byte(nil), r.data[lo:min(lo+r.l.ShardSize, int64(len(r.data)))]...)
	if r.corrupt[i] {
		b[0] ^= 0xff
	}
	return b, nil
}

func (r *memReader) ParityShard(i int64) ([]byte, error) {
	r.parReads++
	if r.lostPar[i] {
		return nil, errors.New("EIO")
	}
	lo := i * r.l.ShardSize
	return r.parity[lo : lo+r.l.ShardSize], nil
}

// encodeWindow returns layout, data, parity, chunk hashes, parity hashes.
func encodeWindow(t *testing.T, size int64, m int) (*Layout, []byte, []byte, []string, []string) {
	t.Helper()
	data := make([]byte, size)
	rand.Read(data)
	l := ForFile(size, testChunk, m)
	if l == nil || l.Scheme != SchemeWindow {
		t.Fatalf("layout = %+v", l)
	}
	var out bytes.Buffer
	e, err := NewWindowEncoder(l, 0, &out)
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	for lo := int64(0); lo < size; lo += testChunk {
		c := data[lo:min(lo+testChunk, size)]
		hashes = append(hashes, hashHex(c))
		if err := e.Add(c); err != nil {
			t.Fatal(err)
		}
	}
	if int64(out.Len()) != l.ParitySize() || int64(len(e.Hashes())) != l.ParityShards() {
		t.Fatalf("parity %d bytes / %d hashes, layout says %d / %d", out.Len(), len(e.Hashes()), l.ParitySize(), l.ParityShards())
	}
	return l, data, out.Bytes(), hashes, e.Hashes()
}

func set(idx ...int64) map[int64]bool {
	m := map[int64]bool{}
	for _, i := range idx {
		m[i] = true
	}
	return m
}

// repairAll runs RepairWindow and collects the rebuilt chunks.
func repairAll(l *Layout, bad []int64, ch, ph []string, r Reader) (map[int64][]byte, error) {
	got := map[int64][]byte{}
	err := RepairWindow(l, bad, ch, ph, r, func(i int64, b []byte) error {
		got[i] = append([]byte(nil), b...)
		return nil
	})
	return got, err
}

func checkRepair(t *testing.T, l *Layout, data []byte, got map[int64][]byte) {
	t.Helper()
	for i, b := range got {
		lo := i * l.ShardSize
		if !bytes.Equal(b, data[lo:min(lo+l.ShardSize, int64(len(data)))]) {
			t.Fatalf("chunk %d rebuilt wrong", i)
		}
	}
}

func TestShards(t *testing.T) {
	for _, c := range []struct{ pct, m int }{{0, 0}, {5, 1}, {10, 2}, {12, 3}, {25, 5}} {
		if m, err := Shards(c.pct); err != nil || m != c.m {
			t.Errorf("Shards(%d) = %d, %v", c.pct, m, err)
		}
	}
	for _, bad := range []int{-1, 26, 100} {
		if _, err := Shards(bad); err == nil {
			t.Errorf("Shards(%d) accepted", bad)
		}
	}
}

// A contiguous burst of M*D chunks in one window is recoverable thanks to
// interleaving.
func TestWindowRepairsBurst(t *testing.T) {
	// Three full windows plus a partial one with a short last chunk.
	size := int64(3*K*D*testChunk + 7*testChunk + 5)
	l, data, par, ch, ph := encodeWindow(t, size, 2)
	var bad []int64
	for i := int64(K*D + 10); i < int64(K*D+10+2*D); i++ { // 32 chunks inside window 1
		bad = append(bad, i)
	}
	r := &memReader{data: data, parity: par, l: l, lost: set(bad...)}
	got, err := repairAll(l, bad, ch, ph, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(bad) {
		t.Fatalf("rebuilt %d of %d", len(got), len(bad))
	}
	checkRepair(t, l, data, got)
}

func TestWindowRepairsLastPartialChunk(t *testing.T) {
	size := int64(K*D*testChunk + 3*testChunk + 5)
	l, data, par, ch, ph := encodeWindow(t, size, 1)
	last := l.Chunks() - 1
	r := &memReader{data: data, parity: par, l: l, corrupt: set(last)}
	got, err := repairAll(l, []int64{last}, ch, ph, r)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got[last])) != 5 {
		t.Fatalf("last chunk length %d", len(got[last]))
	}
	checkRepair(t, l, data, got)
}

func TestWindowTooMuchDamage(t *testing.T) {
	size := int64(K * D * testChunk)
	l, data, par, ch, ph := encodeWindow(t, size, 1)
	// Chunks 0 and D share stripe 0; one parity shard cannot cover both.
	r := &memReader{data: data, parity: par, l: l, lost: set(0, D)}
	if _, err := repairAll(l, []int64{0, D}, ch, ph, r); !errors.Is(err, ErrUnrecoverable) {
		t.Fatalf("err = %v", err)
	}
}

// Damaged parity counts as missing; it must never produce wrong data.
func TestWindowDamagedParity(t *testing.T) {
	size := int64(K * D * testChunk)
	l, data, par, ch, ph := encodeWindow(t, size, 2)
	r := &memReader{data: data, parity: par, l: l, lost: set(3), lostPar: set(l.parityIndex(0, 3, 0))}
	got, err := repairAll(l, []int64{3}, ch, ph, r)
	if err != nil {
		t.Fatal(err)
	}
	checkRepair(t, l, data, got)

	corrupted := append([]byte(nil), par...)
	corrupted[l.parityIndex(0, 3, 1)*testChunk] ^= 1
	r = &memReader{data: data, parity: corrupted, l: l, lost: set(3), lostPar: set(l.parityIndex(0, 3, 0))}
	if _, err := repairAll(l, []int64{3}, ch, ph, r); !errors.Is(err, ErrUnrecoverable) {
		t.Fatalf("err = %v", err)
	}
}

func TestWindowEncoderResume(t *testing.T) {
	size := int64(2*K*D*testChunk + 40*testChunk)
	l, data, par, _, ph := encodeWindow(t, size, 2)

	// Encode only from the second window on, as after a resume.
	var out bytes.Buffer
	e, err := NewWindowEncoder(l, K*D, &out)
	if err != nil {
		t.Fatal(err)
	}
	e.SetHashes(ph[:D*2])
	for lo := int64(K * D * testChunk); lo < size; lo += testChunk {
		e.Add(data[lo:min(lo+testChunk, size)])
	}
	if !bytes.Equal(out.Bytes(), par[D*2*testChunk:]) || len(e.Hashes()) != len(ph) {
		t.Fatal("resumed parity differs")
	}
	if _, err := NewWindowEncoder(l, 5, &out); err == nil {
		t.Fatal("resume inside a window accepted")
	}
}

func TestSmallRepair(t *testing.T) {
	for _, size := range []int64{1, 19, 20, 21, 250, K*testChunk - 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := make([]byte, size)
			rand.Read(data)
			l := ForFile(size, testChunk, 2)
			if l.Scheme != SchemeSmall {
				t.Fatalf("scheme %s", l.Scheme)
			}
			par, dh, ph, err := EncodeSmall(l, data)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(par)) != l.ParitySize() {
				t.Fatalf("parity %d, want %d", len(par), l.ParitySize())
			}
			damaged := append([]byte(nil), data...)
			damaged[0] ^= 0xff
			damaged[size-1] ^= 0x0f
			shards := [][]byte{par[:l.ShardSize], par[l.ShardSize:]}
			got, err := RepairSmall(l, damaged, dh, shards, ph)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatal("rebuilt data differs")
			}
		})
	}
}

func TestSmallTooMuchDamage(t *testing.T) {
	data := make([]byte, 200)
	rand.Read(data)
	l := ForFile(200, testChunk, 1)
	par, dh, ph, _ := EncodeSmall(l, data)
	damaged := append([]byte(nil), data...)
	damaged[0] ^= 1
	damaged[199] ^= 1
	if _, err := RepairSmall(l, damaged, dh, [][]byte{par}, ph); !errors.Is(err, ErrUnrecoverable) {
		t.Fatalf("err = %v", err)
	}
}

func TestLayoutValidate(t *testing.T) {
	good := []*Layout{ForFile(K*testChunk, testChunk, 2), ForFile(100, testChunk, 2)}
	for _, l := range good {
		if err := l.Validate(); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
	bad := []Layout{
		{Scheme: "xor", K: K, M: 1, ShardSize: 16, Size: 100},
		{Scheme: SchemeSmall, K: 0, M: 1, ShardSize: 16, Size: 100},
		{Scheme: SchemeSmall, K: K, M: 1, ShardSize: 1 << 40, Size: 100},
		{Scheme: SchemeSmall, K: K, M: 1, ShardSize: 6, Size: 100},
		{Scheme: SchemeWindow, K: K, M: 1, D: 0, ShardSize: 16, Size: 1 << 20},
		{Scheme: SchemeWindow, K: K, M: 1, D: D, ShardSize: 16, Size: 10},
		{Scheme: SchemeSmall, K: K, M: 1, ShardSize: 5, Size: -1},
		{Scheme: SchemeWindow, K: 128, M: 128, D: D, ShardSize: 16, Size: 1 << 30},
		{Scheme: SchemeWindow, K: K, M: 2, D: D, ShardSize: 256 << 20, Size: 1 << 40},
		{Scheme: SchemeSmall, K: K, M: 2, ShardSize: 1 << 30, Size: 20 << 30},
		{Scheme: SchemeWindow, K: K, M: 2, D: D, ShardSize: 16, Size: 1 << 62},
	}
	for _, l := range bad {
		if err := l.Validate(); err == nil {
			t.Errorf("accepted %+v", l)
		}
	}
	if ForFile(0, testChunk, 2) != nil || ForFile(100, testChunk, 0) != nil {
		t.Error("parity for an empty file or with parity disabled")
	}
}
