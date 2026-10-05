package parity

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"testing"
)

// Layouts come from tape. Every layout that validates must give sane,
// non-overflowing sizes, and every chunk must map into its window.
func FuzzLayout(f *testing.F) {
	f.Add(true, 20, 4, 16, int64(4<<20), int64(1<<40))
	f.Add(false, 20, 2, 0, int64(5), int64(100))
	f.Fuzz(func(t *testing.T, window bool, k, m, d int, shard, size int64) {
		l := &Layout{Scheme: SchemeSmall, K: k, M: m, D: d, ShardSize: shard, Size: size}
		if window {
			l.Scheme = SchemeWindow
		}
		if l.Validate() != nil {
			return
		}
		n, ps := l.ParityShards(), l.ParitySize()
		if n < int64(l.M) || ps < 0 || ps/l.ShardSize != n {
			t.Fatalf("%+v: %d shards, %d bytes", l, n, ps)
		}
		if !window {
			return
		}
		chunks := l.Chunks()
		for _, i := range []int64{0, chunks - 1, chunks / 2, rand.Int64N(chunks)} {
			w, s, pos := l.locate(i)
			win := l.window(w)
			if s >= win.stripes || pos >= dataShards(win, s) || i < win.first || i >= win.first+win.count {
				t.Fatalf("%+v: chunk %d -> window %d stripe %d pos %d (%+v)", l, i, w, s, pos, win)
			}
			if idx := l.parityIndex(w, s, l.M-1); idx < 0 || idx >= n {
				t.Fatalf("%+v: parity index %d of %d", l, idx, n)
			}
		}
	})
}

type fuzzReader struct {
	chunks [][]byte
	parity [][]byte
}

func (r *fuzzReader) Chunk(i int64) ([]byte, error) {
	if r.chunks[i] == nil {
		return nil, errors.New("unreadable")
	}
	return r.chunks[i], nil
}

func (r *fuzzReader) ParityShard(i int64) ([]byte, error) {
	if r.parity[i] == nil {
		return nil, errors.New("unreadable")
	}
	return r.parity[i], nil
}

// Encode, damage, repair: the result is the original data or a refusal,
// never wrong data, and damage within M shards per stripe is always fixed.
func FuzzRepairWindow(f *testing.F) {
	f.Add(uint64(1), uint8(3), uint8(2), uint8(2), uint16(7), uint16(200), []byte{1, 5, 9}, []byte{0})
	f.Fuzz(func(t *testing.T, seed uint64, k, m, d uint8, shard, size uint16, badChunks, badParity []byte) {
		l := &Layout{Scheme: SchemeWindow, K: int(k%8) + 1, M: int(m%4) + 1, D: int(d%4) + 1, ShardSize: int64(shard%64) + 1}
		l.Size = int64(l.K)*l.ShardSize + int64(size)%(4*int64(l.K*l.D)*l.ShardSize)
		if l.Validate() != nil {
			t.Fatalf("generated invalid layout %+v", l)
		}
		rng := rand.New(rand.NewPCG(seed, 7))
		data := make([]byte, l.Size)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		var out bytes.Buffer
		enc, err := NewWindowEncoder(l, 0, &out)
		if err != nil {
			t.Fatal(err)
		}
		r := &fuzzReader{}
		var hashes []string
		for i := int64(0); i < l.Chunks(); i++ {
			c := data[i*l.ShardSize : min((i+1)*l.ShardSize, l.Size)]
			if err := enc.Add(c); err != nil {
				t.Fatal(err)
			}
			r.chunks = append(r.chunks, bytes.Clone(c))
			hashes = append(hashes, hashHex(c))
		}
		if int64(len(enc.Hashes())) != l.ParityShards() || int64(out.Len()) != l.ParitySize() {
			t.Fatalf("%+v: %d parity hashes, %d bytes", l, len(enc.Hashes()), out.Len())
		}
		for i := int64(0); i < l.ParityShards(); i++ {
			r.parity = append(r.parity, bytes.Clone(out.Bytes()[i*l.ShardSize:(i+1)*l.ShardSize]))
		}

		// Damage: listed chunks are unreadable or flipped; listed parity
		// shards are lost.
		var bad []int64
		lost := map[[2]int64]int{} // (window, stripe) -> shards lost
		isBad := map[int64]bool{}
		for j, b := range badChunks {
			i := int64(b) % l.Chunks()
			if isBad[i] {
				continue
			}
			isBad[i] = true
			bad = append(bad, i)
			if j%2 == 0 {
				r.chunks[i] = nil
			} else {
				r.chunks[i][0] ^= 0xFF
			}
			w, s, _ := l.locate(i)
			lost[[2]int64{w, int64(s)}]++
		}
		lostParity := map[int64]bool{}
		for _, b := range badParity {
			i := int64(b) % l.ParityShards()
			if !lostParity[i] {
				lostParity[i] = true
				r.parity[i] = nil
			}
		}
		recoverable := true
		for key, n := range lost {
			for j := range l.M {
				if lostParity[l.parityIndex(key[0], int(key[1]), j)] {
					n++
				}
			}
			if n > l.M {
				recoverable = false
			}
		}

		got := map[int64][]byte{}
		err = RepairWindow(l, bad, hashes, enc.Hashes(), r, func(i int64, c []byte) error {
			got[i] = bytes.Clone(c)
			return nil
		})
		if recoverable && err != nil {
			t.Fatalf("%+v: recoverable damage %v not repaired: %v", l, bad, err)
		}
		if err != nil && !errors.Is(err, ErrUnrecoverable) {
			t.Fatalf("%+v: unexpected error %v", l, err)
		}
		for i, c := range got {
			if !bytes.Equal(c, data[i*l.ShardSize:min((i+1)*l.ShardSize, l.Size)]) {
				t.Fatalf("%+v: chunk %d rebuilt wrong", l, i)
			}
		}
	})
}

func FuzzRepairSmall(f *testing.F) {
	f.Add(uint64(1), uint8(3), uint16(50), []byte{1}, []byte{})
	f.Fuzz(func(t *testing.T, seed uint64, m uint8, size uint16, badShards, badParity []byte) {
		size16 := int64(size%4000) + 1
		l := &Layout{Scheme: SchemeSmall, K: K, M: int(m%maxM) + 1, ShardSize: (size16 + K - 1) / K, Size: size16}
		if l.Validate() != nil {
			t.Fatalf("invalid %+v", l)
		}
		rng := rand.New(rand.NewPCG(seed, 9))
		data := make([]byte, l.Size)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		par, dh, ph, err := EncodeSmall(l, data)
		if err != nil {
			t.Fatal(err)
		}
		damaged := bytes.Clone(data)
		lost := map[int]bool{}
		for _, b := range badShards {
			i := int(b) % l.K
			off := int64(i) * l.ShardSize
			if off < l.Size {
				damaged[off] ^= 0xA5
				lost[i] = true
			}
		}
		shards := make([][]byte, l.M)
		for j := range shards {
			shards[j] = par[int64(j)*l.ShardSize : int64(j+1)*l.ShardSize]
		}
		for _, b := range badParity {
			j := int(b) % l.M
			if shards[j] != nil {
				shards[j] = nil
				lost[l.K+j] = true
			}
		}
		out, err := RepairSmall(l, damaged, dh, shards, ph)
		if len(lost) <= l.M && err != nil {
			t.Fatalf("%+v: %d lost shards not repaired: %v", l, len(lost), err)
		}
		if err == nil && !bytes.Equal(out, data) {
			t.Fatalf("%+v: repaired data differs", l)
		}
	})
}
