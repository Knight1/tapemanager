package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := run(args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCLIWorkflow(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "dl")
	tape := filepath.Join(base, "tape")
	cat := filepath.Join(base, "cat")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.MkdirAll(tape, 0o755)
	os.WriteFile(filepath.Join(src, "sub", "ubuntu.iso"), []byte("data"), 0o644)
	common := []string{"--tape", tape, "--catalog", cat, "--no-mount-check"}

	if code, _, errOut := runCmd(t, append([]string{"archive", "put", "--label", "T1"}, append(common, src)...)...); code != 0 {
		t.Fatalf("put: %d %s", code, errOut)
	}
	if code, out, _ := runCmd(t, append([]string{"archive", "verify"}, common...)...); code != 0 || !strings.Contains(out, "successful") {
		t.Fatalf("verify: %d %s", code, out)
	}
	if code, out, _ := runCmd(t, append([]string{"archive", "list"}, common...)...); code != 0 || !strings.Contains(out, "dl/sub/ubuntu.iso") {
		t.Fatalf("list: %d %s", code, out)
	}
	if code, out, _ := runCmd(t, "catalog", "tapes", "--catalog", cat); code != 0 || !strings.Contains(out, "T1") || !strings.Contains(out, "OK") {
		t.Fatalf("tapes: %d %s", code, out)
	}
	if code, out, _ := runCmd(t, "catalog", "search", "--catalog", cat, "ubuntu"); code != 0 || !strings.Contains(out, "ubuntu.iso") {
		t.Fatalf("search: %d %s", code, out)
	}
	if code, _, _ := runCmd(t, "catalog", "search", "--catalog", cat, "nothing-here"); code != exitFailure {
		t.Fatalf("search without hits exit = %d", code)
	}
	if code, _, _ := runCmd(t, append([]string{"catalog", "import"}, common...)...); code != 0 {
		t.Fatalf("import exit = %d", code)
	}

	// Corruption makes verify fail with exit 1.
	os.WriteFile(filepath.Join(tape, "dl", "sub", "ubuntu.iso"), []byte("DATA"), 0o644)
	if code, out, _ := runCmd(t, append([]string{"archive", "verify"}, common...)...); code != exitFailure || !strings.Contains(out, "FAILED") {
		t.Fatalf("verify after corruption: %d %s", code, out)
	}
}

func TestCLIMountCheck(t *testing.T) {
	code, _, errOut := runCmd(t, "archive", "verify", "--tape", t.TempDir(), "--catalog", t.TempDir())
	if code != exitFailure || !strings.Contains(errOut, "not an LTFS") {
		t.Fatalf("%d %s", code, errOut)
	}
}

func TestCLIUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"archive"}, {"archive", "put"}, {"catalog", "search"}} {
		if code, _, _ := runCmd(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
	if code, out, _ := runCmd(t, "version"); code != 0 || !strings.Contains(out, "tapemgr") {
		t.Errorf("version: %d %s", code, out)
	}
}

func runWithInput(input string, args ...string) (int, string) {
	var out, errOut strings.Builder
	code := run(args, strings.NewReader(input), &out, &errOut)
	return code, out.String() + errOut.String()
}

func TestCLIPurgeConfirmation(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "dl")
	tape := filepath.Join(base, "tape")
	cat := filepath.Join(base, "cat")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(tape, 0o755)
	file := filepath.Join(src, "a.iso")
	os.WriteFile(file, []byte("data"), 0o644)
	common := []string{"--tape", tape, "--catalog", cat, "--no-mount-check"}
	purge := append([]string{"archive", "purge-source"}, append(common, src)...)

	runCmd(t, append([]string{"archive", "put"}, append(common, src)...)...)

	if code, out := runWithInput("y\n", purge...); code != exitFailure || !strings.Contains(out, "not passed verification") {
		t.Fatalf("purge before verify: %d %s", code, out)
	}
	runCmd(t, append([]string{"archive", "verify"}, common...)...)

	for _, answer := range []string{"\n", "n\n", "no\n", ""} {
		if code, out := runWithInput(answer, purge...); code != exitFailure || !strings.Contains(out, "Aborted") {
			t.Fatalf("answer %q: %d %s", answer, code, out)
		}
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("file deleted without confirmation")
	}
	code, out := runWithInput("y\n", purge...)
	if code != exitOK || !strings.Contains(out, "This will delete 1 files") || !strings.Contains(out, "Deleted 1 of 1") {
		t.Fatalf("confirmed purge: %d %s", code, out)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("file not deleted")
	}
}

func TestCLIPurgeYesAndRecover(t *testing.T) {
	base := t.TempDir()
	tape := filepath.Join(base, "tape")
	cat := filepath.Join(base, "cat")
	os.MkdirAll(tape, 0o755)
	os.WriteFile(filepath.Join(tape, "copied.bin"), []byte("x"), 0o644)
	common := []string{"--tape", tape, "--catalog", cat, "--no-mount-check"}

	code, out := runWithInput("", append([]string{"archive", "recover", "--label", "OLD"}, common...)...)
	if code != exitOK || !strings.Contains(out, "Recovered 1 files") {
		t.Fatalf("recover: %d %s", code, out)
	}
	if code, out := runWithInput("", append([]string{"archive", "list"}, common...)...); code != 0 || !strings.Contains(out, "copied.bin  (recovered)") {
		t.Fatalf("list: %d %s", code, out)
	}

	src := filepath.Join(base, "src")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "f"), []byte("f"), 0o644)
	runCmd(t, append([]string{"archive", "put"}, append(common, src)...)...)
	runCmd(t, append([]string{"archive", "verify"}, common...)...)
	if code, out := runWithInput("", append([]string{"archive", "purge-source", "--yes"}, append(common, src)...)...); code != exitOK {
		t.Fatalf("purge --yes: %d %s", code, out)
	}
}

func TestCLIRestoreRepairs(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "dl")
	tape := filepath.Join(base, "tape")
	cat := filepath.Join(base, "cat")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(tape, 0o755)
	data := strings.Repeat("0123456789", 50)
	os.WriteFile(filepath.Join(src, "f.txt"), []byte(data), 0o644)
	common := []string{"--tape", tape, "--catalog", cat, "--no-mount-check"}
	runCmd(t, append([]string{"archive", "put", "--parity", "10"}, append(common, src)...)...)

	// Damage the first bytes on tape; small-file parity rebuilds them.
	f, _ := os.OpenFile(filepath.Join(tape, "dl", "f.txt"), os.O_WRONLY, 0)
	f.WriteAt([]byte("XXXX"), 0)
	f.Close()

	if code, out, _ := runCmd(t, append([]string{"archive", "verify"}, common...)...); code != exitFailure || !strings.Contains(out, "Repairable:  1") {
		t.Fatalf("verify: %d %s", code, out)
	}
	out := filepath.Join(base, "out")
	code, stdout, errOut := runCmd(t, append([]string{"archive", "restore", "--to", out}, common...)...)
	if code != exitOK || !strings.Contains(stdout, "1 repaired") {
		t.Fatalf("restore: %d %s %s", code, stdout, errOut)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "dl", "f.txt")); string(got) != data {
		t.Fatal("restored content differs")
	}
	if code, _, _ := runCmd(t, append([]string{"archive", "restore"}, common...)...); code != exitUsage {
		t.Fatal("restore without --to accepted")
	}
}
