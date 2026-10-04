package parity

import (
	"errors"
	"fmt"
)

// ErrUnrecoverable means a stripe lost more shards than it has parity.
var ErrUnrecoverable = errors.New("too much damage to repair")

// Reader supplies shards for repair. A returned error marks the shard as
// missing. Shards are checked against their hashes before use, so a reader
// may return damaged data; it is treated as missing.
type Reader interface {
	Chunk(i int64) ([]byte, error)       // data chunk i, unpadded
	ParityShard(i int64) ([]byte, error) // parity shard i of this file
}

// RepairWindow rebuilds the given bad chunks of a window-layout file and
// returns them by index. chunkHashes and parityHashes are the recorded
// hashes of all data chunks and parity shards.
func RepairWindow(l *Layout, bad []int64, chunkHashes, parityHashes []string, r Reader) (map[int64][]byte, error) {
	if l.Scheme != SchemeWindow {
		return nil, errors.New("not a window layout")
	}
	if int64(len(chunkHashes)) != l.Chunks() || int64(len(parityHashes)) != l.ParityShards() {
		return nil, errors.New("hash lists do not match the layout")
	}
	type key struct {
		w int64
		s int
	}
	stripes := map[key]bool{}
	for _, i := range bad {
		if i < 0 || i >= l.Chunks() {
			return nil, fmt.Errorf("chunk %d out of range", i)
		}
		w, s, _ := l.locate(i)
		stripes[key{w, s}] = true
	}

	out := map[int64][]byte{}
	for k := range stripes {
		win := l.window(k.w)
		n := dataShards(win, k.s)
		shards := make([][]byte, n+l.M)
		missing := 0
		for pos := range n {
			i := win.first + int64(k.s) + int64(pos)*int64(win.stripes)
			b, err := r.Chunk(i)
			if err != nil || hashHex(b) != chunkHashes[i] {
				missing++
				continue
			}
			shards[pos] = pad(b, l.ShardSize)
		}
		for j := range l.M {
			idx := l.parityIndex(k.w, k.s, j)
			b, err := r.ParityShard(idx)
			if err != nil || int64(len(b)) != l.ShardSize || hashHex(b) != parityHashes[idx] {
				missing++
				continue
			}
			shards[n+j] = b
		}
		if missing > l.M {
			return nil, fmt.Errorf("%w: %d of %d shards lost in one stripe, %d parity", ErrUnrecoverable, missing, n+l.M, l.M)
		}
		c, err := newCoder(n, l.M)
		if err != nil {
			return nil, err
		}
		if err := c.ReconstructData(shards); err != nil {
			return nil, err
		}
		for pos := range n {
			i := win.first + int64(k.s) + int64(pos)*int64(win.stripes)
			size := min(l.ShardSize, l.Size-i*l.ShardSize)
			chunk := shards[pos][:size]
			if hashHex(chunk) != chunkHashes[i] {
				return nil, fmt.Errorf("chunk %d: rebuilt data does not match its hash", i)
			}
			out[i] = chunk
		}
	}
	result := map[int64][]byte{}
	for _, i := range bad {
		result[i] = out[i]
	}
	return result, nil
}

// RepairSmall rebuilds a small file. data is the file as read, with
// unreadable bytes in any state; damaged shards are found through
// dataHashes. paritySh holds the parity shards as read (nil if unreadable).
func RepairSmall(l *Layout, data []byte, dataHashes []string, paritySh [][]byte, parityHashes []string) ([]byte, error) {
	if l.Scheme != SchemeSmall || int64(len(data)) != l.Size {
		return nil, errors.New("layout does not match data")
	}
	if len(dataHashes) != l.K || len(paritySh) != l.M || len(parityHashes) != l.M {
		return nil, errors.New("hash lists do not match the layout")
	}
	shards := splitSmall(l, data)
	missing := 0
	for i := range l.K {
		if hashHex(shards[i]) != dataHashes[i] {
			shards[i] = nil
			missing++
		}
	}
	for j := range l.M {
		b := paritySh[j]
		if int64(len(b)) != l.ShardSize || hashHex(b) != parityHashes[j] {
			shards[l.K+j] = nil
			missing++
			continue
		}
		shards[l.K+j] = b
	}
	if missing > l.M {
		return nil, fmt.Errorf("%w: %d of %d shards lost, %d parity", ErrUnrecoverable, missing, l.K+l.M, l.M)
	}
	c, err := newCoder(l.K, l.M)
	if err != nil {
		return nil, err
	}
	if err := c.ReconstructData(shards); err != nil {
		return nil, err
	}
	out := make([]byte, 0, l.Size)
	for i := range l.K {
		if hashHex(shards[i]) != dataHashes[i] {
			return nil, fmt.Errorf("shard %d: rebuilt data does not match its hash", i)
		}
		out = append(out, shards[i]...)
	}
	return out[:l.Size], nil
}

func pad(b []byte, size int64) []byte {
	if int64(len(b)) == size {
		return b
	}
	p := make([]byte, size)
	copy(p, b)
	return p
}
