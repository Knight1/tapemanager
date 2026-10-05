package main

import (
	"fmt"
	"io"
)

// termSafe escapes control characters on their way to the terminal. File
// names and labels come from tapes and may contain escape sequences that
// rewrite the screen, hide lines or set the clipboard. Newlines, tabs and
// carriage returns (progress bars) pass through; everything else below
// 0x20, DEL and the C1 controls (U+0080 to U+009F) are shown as \xNN.
type termSafe struct{ w io.Writer }

func unsafeAt(p []byte, i int) int {
	switch c := p[i]; {
	case c == '\n' || c == '\r' || c == '\t':
		return 0
	case c < 0x20 || c == 0x7F:
		return 1
	case c == 0xC2 && i+1 < len(p) && p[i+1] >= 0x80 && p[i+1] <= 0x9F:
		return 2
	}
	return 0
}

func (t termSafe) Write(p []byte) (int, error) {
	i := 0
	for i < len(p) && unsafeAt(p, i) == 0 {
		i++
	}
	if i == len(p) {
		return t.w.Write(p)
	}
	out := make([]byte, 0, len(p)+16)
	for i := 0; i < len(p); {
		switch n := unsafeAt(p, i); n {
		case 0:
			out = append(out, p[i])
			i++
		default:
			for _, c := range p[i : i+n] {
				out = fmt.Appendf(out, `\x%02x`, c)
			}
			i += n
		}
	}
	if _, err := t.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Unwrap returns the underlying writer, so terminal detection still works.
func (t termSafe) Unwrap() io.Writer { return t.w }
