package main

import (
	"os"
	"strings"
	"testing"
)

func TestTermSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain line\n":                   "plain line\n",
		"tab\tand\rprogress":             "tab\tand\rprogress",
		"\x1b]52;c;cHduZWQ=\x07evil.iso": `\x1b]52;c;cHduZWQ=\x07evil.iso`,
		"\x1b[2Kfake OK":                 `\x1b[2Kfake OK`,
		"c1 \u009b31m":                   `c1 \xc2\x9b31m`,
		"del\x7f":                        `del\x7f`,
		"umlaut ä stays":                 "umlaut ä stays",
	} {
		var b strings.Builder
		n, err := termSafe{&b}.Write([]byte(in))
		if err != nil || n != len(in) || b.String() != want {
			t.Errorf("%q: got %q (%d, %v), want %q", in, b.String(), n, err, want)
		}
	}
	if progressOut(termSafe{os.Stderr}) == nil && progressOut(os.Stderr) != nil {
		t.Error("terminal detection does not see through termSafe")
	}
}

// Names from a crafted tape reach the terminal escaped through the whole
// CLI path.
func TestListEscapesTapeNames(t *testing.T) {
	base := t.TempDir()
	src := base + "/src"
	tape := base + "/tape"
	os.MkdirAll(src, 0o755)
	os.MkdirAll(tape, 0o755)
	os.WriteFile(src+"/a\x1b[2Kb", []byte("x"), 0o644)
	common := []string{"--tape", tape, "--catalog", base + "/cat", "--no-mount-check"}
	if code, _, errOut := runCmd(t, append(append([]string{"archive", "put"}, common...), "--parity", "0", src)...); code != 0 {
		t.Fatalf("put: %s", errOut)
	}
	var out, errOut strings.Builder
	run(append([]string{"archive", "list"}, common...), strings.NewReader(""), termSafe{&out}, termSafe{&errOut})
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), `a\x1b[2Kb`) {
		t.Fatalf("list output %q", out.String())
	}
}
