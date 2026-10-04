package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/Knight1/tapemanager/internal/manifest"
	"github.com/Knight1/tapemanager/internal/parity"
)

// tapeFile is an archived file with everything known about it.
type tapeFile struct {
	tape   *manifest.Tape
	entry  manifest.Entry
	chunks *manifest.Chunks // nil if unknown
	parity *manifest.Parity // nil if none
	f      *os.File
}

func openTapeFile(tape *manifest.Tape, e manifest.Entry, chunks map[string]manifest.Chunks, par map[string]manifest.Parity) (*tapeFile, error) {
	tf := &tapeFile{tape: tape, entry: e}
	if c, ok := chunks[e.Path]; ok {
		tf.chunks = &c
	}
	if p, ok := par[e.Path]; ok && p.Layout.Size == e.Size {
		tf.parity = &p
	}
	f, err := tape.Root().Open(filepath.FromSlash(e.Path))
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil {
		f.Close()
		return nil, err
	} else if !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("not a regular file")
	}
	tf.f = f
	return tf, nil
}

func (tf *tapeFile) Close() error { return tf.f.Close() }

func (tf *tapeFile) chunkSize() int64 {
	if tf.chunks != nil {
		return tf.chunks.ChunkSize
	}
	return DefaultChunkSize
}

// scanResult describes what reading a file back found.
type scanResult struct {
	sum        string  // SHA-256 of what was read; valid only without read errors
	readErrors []int64 // chunks that could not be read
	bad        []int64 // chunks unreadable or not matching their hash
	data       []byte  // whole file, kept for small-file parity repair
}

func (s *scanResult) intact(e manifest.Entry) bool {
	return len(s.readErrors) == 0 && len(s.bad) == 0 && s.sum == e.SHA256
}

// scan reads the file chunk by chunk. A read error marks the chunk and
// reading continues with the next one, so one damaged area does not hide the
// rest of the file. Good chunks are written to sink if it is not nil.
//
// Chunks are read ahead in a separate goroutine and the two hashes of each
// chunk run in parallel, so verification keeps the drive streaming.
func (tf *tapeFile) scan(sink io.WriterAt, prog io.Writer) (*scanResult, error) {
	size, cs := tf.entry.Size, tf.chunkSize()
	res := &scanResult{}
	keep := tf.parity != nil && tf.parity.Layout.Scheme == parity.SchemeSmall
	if keep {
		res.data = make([]byte, size)
	}

	type piece struct {
		i   int64
		buf []byte
		ok  bool
	}
	stop := make(chan struct{})
	defer close(stop)
	pieces := make(chan piece, readAhead)
	free := make(chan []byte, readAhead+2)
	for range readAhead + 2 {
		free <- make([]byte, cs)
	}
	go func() {
		defer close(pieces)
		for i := int64(0); i*cs < size; i++ {
			var buf []byte
			select {
			case buf = <-free:
			case <-stop:
				return
			}
			off := i * cs
			want := min(cs, size-off)
			n, err := tf.f.ReadAt(buf[:want], off)
			ok := int64(n) == want && (err == nil || err == io.EOF)
			select {
			case pieces <- piece{i: i, buf: buf[:want], ok: ok}:
			case <-stop:
				return
			}
		}
	}()

	h := sha256.New()
	for p := range pieces {
		off := p.i * cs
		prog.Write(p.buf)
		if !p.ok {
			res.readErrors = append(res.readErrors, p.i)
			res.bad = append(res.bad, p.i)
			free <- p.buf[:cap(p.buf)]
			continue
		}
		var wg sync.WaitGroup
		var sum [sha256.Size]byte
		wg.Go(func() { h.Write(p.buf) })
		if tf.chunks != nil {
			wg.Go(func() { sum = sha256.Sum256(p.buf) })
		}
		if keep {
			copy(res.data[off:], p.buf)
		}
		wg.Wait()
		good := tf.chunks == nil || (p.i < int64(len(tf.chunks.SHA256)) && hex.EncodeToString(sum[:]) == tf.chunks.SHA256[p.i])
		if !good {
			res.bad = append(res.bad, p.i)
		} else if sink != nil {
			if _, err := sink.WriteAt(p.buf, off); err != nil {
				return nil, err
			}
		}
		free <- p.buf[:cap(p.buf)]
	}
	if len(res.readErrors) == 0 {
		res.sum = hex.EncodeToString(h.Sum(nil))
	}
	return res, nil
}

// repair rebuilds damaged data with parity. For window layouts it returns
// the rebuilt chunks by index; for small files the whole file as index 0.
func (tf *tapeFile) repair(s *scanResult) (map[int64][]byte, error) {
	p := tf.parity
	if p == nil {
		return nil, errors.New("no parity")
	}
	if p.Layout.Scheme == parity.SchemeSmall {
		shards := make([][]byte, p.Layout.M)
		for j := range shards {
			shards[j], _ = tf.tape.ReadParityShard(*p, int64(j))
		}
		data, err := parity.RepairSmall(&p.Layout, s.data, p.DataHashes, shards, p.Hashes)
		if err != nil {
			return nil, err
		}
		return map[int64][]byte{0: data}, nil
	}
	if tf.chunks == nil || tf.chunks.ChunkSize != p.Layout.ShardSize {
		return nil, errors.New("chunk hashes needed to locate damage are missing")
	}
	if len(s.bad) == 0 {
		return nil, errors.New("damage could not be located")
	}
	return parity.RepairWindow(&p.Layout, s.bad, tf.chunks.SHA256, p.Hashes, tf)
}

// Chunk implements parity.Reader.
func (tf *tapeFile) Chunk(i int64) ([]byte, error) {
	cs := tf.parity.Layout.ShardSize
	off := i * cs
	b := make([]byte, min(cs, tf.entry.Size-off))
	if _, err := tf.f.ReadAt(b, off); err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

// ParityShard implements parity.Reader.
func (tf *tapeFile) ParityShard(i int64) ([]byte, error) {
	return tf.tape.ReadParityShard(*tf.parity, i)
}

// describeDamage summarizes a scan for the user.
func describeDamage(s *scanResult, e manifest.Entry, cs int64) string {
	switch {
	case len(s.readErrors) > 0:
		idx := make([]int, len(s.readErrors))
		for i, c := range s.readErrors {
			idx[i] = int(c)
		}
		return fmt.Sprintf("unreadable bytes: %s", badRanges(idx, cs, e.Size))
	case len(s.bad) > 0:
		idx := make([]int, len(s.bad))
		for i, c := range s.bad {
			idx[i] = int(c)
		}
		return fmt.Sprintf("damaged bytes: %s", badRanges(idx, cs, e.Size))
	default:
		return fmt.Sprintf("SHA-256 mismatch: manifest %s, tape %s", e.SHA256, s.sum)
	}
}
