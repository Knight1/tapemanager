package archive

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

// progress counts bytes written and periodically redraws a progress bar.
type progress struct {
	out   io.Writer
	total int64
	done  atomic.Int64
	stop  chan struct{}
	ended chan struct{}
	start time.Time
}

func startProgress(out io.Writer, total int64) *progress {
	p := &progress{out: out, total: total, stop: make(chan struct{}), ended: make(chan struct{}), start: time.Now()}
	if out == nil {
		close(p.ended)
		return p
	}
	go func() {
		defer close(p.ended)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.draw()
			case <-p.stop:
				p.draw()
				fmt.Fprintln(p.out)
				return
			}
		}
	}()
	return p
}

func (p *progress) Write(b []byte) (int, error) {
	p.done.Add(int64(len(b)))
	return len(b), nil
}

func (p *progress) finish() {
	if p.out != nil {
		close(p.stop)
	}
	<-p.ended
}

func (p *progress) draw() {
	const width = 32
	done := p.done.Load()
	frac := 1.0
	if p.total > 0 {
		frac = float64(done) / float64(p.total)
	}
	filled := min(int(frac*width), width)
	rate := float64(done) / time.Since(p.start).Seconds()
	fmt.Fprintf(p.out, "\r       [%s%s] %3.0f%%  %s/s ",
		strings.Repeat("=", filled), strings.Repeat(" ", width-filled),
		frac*100, FormatBytes(int64(rate)))
}

// FormatBytes renders n using binary units.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// FormatDuration renders d as HH:MM:SS.
func FormatDuration(d time.Duration) string {
	s := int64(d.Round(time.Second).Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}
