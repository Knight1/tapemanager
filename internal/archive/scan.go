package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	size   int64 // actual size on tape
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
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("not a regular file")
	}
	tf.f, tf.size = f, st.Size()
	return tf, nil
}

func (tf *tapeFile) Close() error { return tf.f.Close() }

func (tf *tapeFile) chunkSize() int64 {
	if tf.chunks != nil {
		return tf.chunks.ChunkSize
	}
	return DefaultChunkSize
}

func (tf *tapeFile) numChunks() int64 {
	cs := tf.chunkSize()
	return (tf.entry.Size + cs - 1) / cs
}

// scanResult describes what reading a file back found.
type scanResult struct {
	sum        string  // SHA-256 of what was read; valid only if complete
	readErrors []int64 // chunks that could not be read
	bad        []int64 // chunks unreadable or not matching their hash, excluding the tail
	// tail is the first chunk of a missing end of the file (the file on tape
	// is shorter than recorded), or -1. Kept as one number so a crafted size
	// cannot make the scan list billions of chunks.
	tail int64
	data []byte // whole file, kept for small-file parity repair
}

func (s *scanResult) intact(e manifest.Entry) bool {
	return len(s.readErrors) == 0 && len(s.bad) == 0 && s.tail < 0 && s.sum == e.SHA256
}

// scan reads the file chunk by chunk. A read error marks the chunk and
// reading continues with the next one, so one damaged area does not hide the
// rest of the file. Good chunks are written to sink if it is not nil.
//
// Chunks are read ahead in a separate goroutine and the two hashes of each
// chunk run in parallel, so verification keeps the drive streaming.
func (tf *tapeFile) scan(sink io.WriterAt, prog io.Writer) (*scanResult, error) {
	size, cs := tf.entry.Size, tf.chunkSize()
	res := &scanResult{tail: -1}
	keep := tf.parity != nil && tf.parity.Layout.Scheme == parity.SchemeSmall
	if keep {
		res.data = make([]byte, size)
	}
	// Only chunks that lie fully inside the file on tape are read.
	readable := tf.numChunks()
	if tf.size < size {
		readable = tf.size / cs
		res.tail = readable
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
		for i := range readable {
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
	if len(res.readErrors) == 0 && res.tail < 0 {
		res.sum = hex.EncodeToString(h.Sum(nil))
	}
	return res, nil
}

// repair rebuilds damaged data with parity and passes each rebuilt piece to
// emit with its file offset. emit may be nil to only check that repair is
// possible. Memory use stays bounded by one stripe.
func (tf *tapeFile) repair(s *scanResult, emit func(off int64, b []byte) error) error {
	p := tf.parity
	if p == nil {
		return errors.New("no parity")
	}
	if p.Layout.Scheme == parity.SchemeSmall {
		shards := make([][]byte, p.Layout.M)
		for j := range shards {
			shards[j], _ = tf.tape.ReadParityShard(*p, int64(j))
		}
		data, err := parity.RepairSmall(&p.Layout, s.data, p.DataHashes, shards, p.Hashes)
		if err != nil {
			return err
		}
		// Every shard matching its hash is not enough: the file as a whole
		// must match too, or the metadata is inconsistent.
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != tf.entry.SHA256 {
			return errors.New("rebuilt file does not match its SHA-256")
		}
		if emit != nil {
			return emit(0, data)
		}
		return nil
	}

	if tf.chunks == nil || tf.chunks.ChunkSize != p.Layout.ShardSize {
		return errors.New("chunk hashes needed to locate damage are missing")
	}
	bad := s.bad
	if s.tail >= 0 {
		// A missing tail longer than all parity can never be rebuilt;
		// refusing early also keeps the list bounded.
		missing := tf.numChunks() - s.tail
		if missing > p.Layout.ParityShards() {
			return fmt.Errorf("%w: %d chunks missing at the end", parity.ErrUnrecoverable, missing)
		}
		for i := s.tail; i < tf.numChunks(); i++ {
			bad = append(bad, i)
		}
	}
	if len(bad) == 0 {
		return errors.New("damage could not be located")
	}
	var fn func(int64, []byte) error
	if emit != nil {
		fn = func(i int64, b []byte) error { return emit(i*p.Layout.ShardSize, b) }
	}
	return parity.RepairWindow(&p.Layout, bad, tf.chunks.SHA256, p.Hashes, tf, fn)
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
	var parts []string
	ranges := func(list []int64) string {
		idx := make([]int, len(list))
		for i, c := range list {
			idx[i] = int(c)
		}
		return badRanges(idx, cs, e.Size)
	}
	if len(s.readErrors) > 0 {
		parts = append(parts, "unreadable bytes: "+ranges(s.readErrors))
	}
	var corrupt []int64
	unreadable := map[int64]bool{}
	for _, c := range s.readErrors {
		unreadable[c] = true
	}
	for _, c := range s.bad {
		if !unreadable[c] {
			corrupt = append(corrupt, c)
		}
	}
	if len(corrupt) > 0 {
		parts = append(parts, "damaged bytes: "+ranges(corrupt))
	}
	if s.tail >= 0 {
		parts = append(parts, fmt.Sprintf("file is cut short, bytes %d-%d missing", s.tail*cs, e.Size-1))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("SHA-256 mismatch: manifest %s, tape %s", e.SHA256, s.sum)
	}
	return strings.Join(parts, "; ")
}
