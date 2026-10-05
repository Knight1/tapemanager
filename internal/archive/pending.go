package archive

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Knight1/tapemanager/internal/manifest"
)

// The pending log holds manifest records for files that are complete on
// tape but not yet written to a tape manifest segment. It lives on local
// disk, where small appends are cheap, and is flushed to the tape in large
// segments.
type pendingRecord struct {
	Entry  manifest.Entry   `json:"entry"`
	Chunks *manifest.Chunks `json:"chunks,omitempty"`
	Parity *manifest.Parity `json:"parity,omitempty"`
}

func (r pendingRecord) Validate() error {
	if err := r.Entry.Validate(); err != nil {
		return err
	}
	if r.Chunks != nil {
		if r.Chunks.Path != r.Entry.Path {
			return fmt.Errorf("%s: chunk record for %s", r.Entry.Path, r.Chunks.Path)
		}
		if err := r.Chunks.Validate(); err != nil {
			return err
		}
	}
	if r.Parity != nil {
		if r.Parity.Path != r.Entry.Path || r.Parity.Layout.Size != r.Entry.Stored().Size {
			return fmt.Errorf("%s: parity record does not match", r.Entry.Path)
		}
		return r.Parity.Validate()
	}
	return nil
}

type pending struct {
	f           *os.File
	records     []pendingRecord
	bytes       int64 // content bytes since the last flush
	parityBytes int64 // staged parity waiting for the next flush
}

// openPending opens the pending log at name, loading records left behind by
// an interrupted run. A torn last line from a crash is dropped; that file's
// data is still on tape and its journal lets the next run record it again.
func openPending(name string) (*pending, error) {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := manifest.SyncDir(filepath.Dir(name)); err != nil {
		f.Close()
		return nil, err
	}
	p := &pending{f: f}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 256<<20)
	var good int64
	var bad error
	for sc.Scan() {
		if bad != nil {
			f.Close()
			return nil, fmt.Errorf("%s: %w", name, bad)
		}
		line := sc.Bytes()
		var r pendingRecord
		if err := json.Unmarshal(line, &r); err != nil {
			bad = err
			continue
		}
		if err := r.Validate(); err != nil {
			bad = err
			continue
		}
		p.records = append(p.records, r)
		if r.Entry.Ref == nil {
			p.bytes += r.Entry.Stored().Size
		}
		if r.Parity != nil {
			p.parityBytes += r.Parity.Layout.ParitySize()
		}
		good += int64(len(line)) + 1
	}
	if err := sc.Err(); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if good > st.Size() {
		// The last record is complete but a crash cut off its newline.
		// Add it, so the next record does not join that line and make
		// both unreadable.
		if _, err := f.WriteAt([]byte{'\n'}, st.Size()); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
	} else if err := f.Truncate(good); err != nil {
		// Cut off a torn tail so new records start on a clean line.
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(good, 0); err != nil {
		f.Close()
		return nil, err
	}
	return p, nil
}

func (p *pending) add(r pendingRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := p.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := p.f.Sync(); err != nil {
		return err
	}
	p.records = append(p.records, r)
	if r.Entry.Ref == nil {
		p.bytes += r.Entry.Stored().Size
	}
	if r.Parity != nil {
		p.parityBytes += r.Parity.Layout.ParitySize()
	}
	return nil
}

func (p *pending) clear() error {
	if err := p.f.Truncate(0); err != nil {
		return err
	}
	if _, err := p.f.Seek(0, 0); err != nil {
		return err
	}
	p.records, p.bytes, p.parityBytes = nil, 0, 0
	return p.f.Sync()
}

func (p *pending) close() error { return p.f.Close() }

// pendingCount returns how many records wait in the pending log at name.
func pendingCount(name string) (int, error) {
	if _, err := os.Stat(name); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	p, err := openPending(name)
	if err != nil {
		return 0, err
	}
	defer p.close()
	return len(p.records), nil
}
