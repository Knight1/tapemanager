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
	code := run(args, &out, &errOut)
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
