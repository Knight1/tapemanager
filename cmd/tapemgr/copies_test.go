package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLISecondCopyAndRetire(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	tape1 := filepath.Join(base, "tape1")
	tape2 := filepath.Join(base, "tape2")
	cat := filepath.Join(base, "cat")
	for _, d := range []string{src, tape1, tape2} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(src, "f.iso"), []byte("content"), 0o644)
	on := func(tape string) []string { return []string{"--tape", tape, "--catalog", cat, "--no-mount-check"} }
	cmd := func(args ...string) (int, string, string) { return runCmd(t, args...) }

	if code, _, errOut := cmd(append(append([]string{"archive", "put", "--label", "ONE"}, on(tape1)...), src)...); code != 0 {
		t.Fatalf("put: %s", errOut)
	}
	// Without --copies the next tape gets nothing: the file is on ONE.
	code, out, _ := cmd(append(append([]string{"archive", "put", "--label", "TWO"}, on(tape2)...), src)...)
	if code != 0 || !strings.Contains(out, "already on tape ONE") || !strings.Contains(out, "1 on other tapes") {
		t.Fatalf("incremental: %d %s", code, out)
	}
	code, out, _ = cmd(append(append([]string{"archive", "put", "--copies", "2"}, on(tape2)...), src)...)
	if code != 0 || !strings.Contains(out, "Files:       1") {
		t.Fatalf("second copy: %d %s", code, out)
	}
	cmd(append([]string{"archive", "verify"}, on(tape1)...)...)

	// Purge with two copies waits for the second verification.
	code, out = runWithInput("", append(append([]string{"archive", "purge-source", "--yes", "--copies", "2"}, on(tape1)...), src)...)
	if !strings.Contains(out, "only 1 of 2") || !exists(filepath.Join(src, "f.iso")) {
		t.Fatalf("purge before second verify: %d %s", code, out)
	}

	if code, out, _ := cmd("catalog", "retire", "--catalog", cat, "ONE"); code != 0 || !strings.Contains(out, "is retired") {
		t.Fatalf("retire: %d %s", code, out)
	}
	if code, out, _ := cmd("catalog", "tapes", "--catalog", cat); code != 0 || !strings.Contains(out, "RETIRED") {
		t.Fatalf("tapes: %d %s", code, out)
	}
	if code, out, _ := cmd("catalog", "search", "--catalog", cat, "f.iso"); code != 0 || !strings.Contains(out, "ONE") || !strings.Contains(out, "(retired)") {
		t.Fatalf("search: %d %s", code, out)
	}
	if code, out, _ := cmd("catalog", "retire", "--catalog", cat, "--undo", "ONE"); code != 0 || !strings.Contains(out, "counts as a copy again") {
		t.Fatalf("undo: %d %s", code, out)
	}
	if code, _, errOut := cmd("catalog", "retire", "--catalog", cat, "NOPE"); code != exitFailure || !strings.Contains(errOut, "no tape") {
		t.Fatalf("unknown: %d %s", code, errOut)
	}
	if code, _, _ := cmd("catalog", "retire", "--catalog", cat); code != exitUsage {
		t.Fatalf("missing argument: %d", code)
	}

	cmd(append([]string{"archive", "verify"}, on(tape2)...)...)
	code, out = runWithInput("", append(append([]string{"archive", "purge-source", "--yes", "--copies", "2"}, on(tape1)...), src)...)
	if code != 0 || exists(filepath.Join(src, "f.iso")) {
		t.Fatalf("purge with two verified copies: %d %s", code, out)
	}
}

func TestCLIRetireAmbiguousLabel(t *testing.T) {
	base := t.TempDir()
	cat := filepath.Join(base, "cat")
	for i, name := range []string{"t1", "t2"} {
		tape := filepath.Join(base, name)
		src := filepath.Join(base, "src"+name)
		os.MkdirAll(tape, 0o755)
		os.MkdirAll(src, 0o755)
		os.WriteFile(filepath.Join(src, "f"), []byte{byte('a' + i)}, 0o644)
		runCmd(t, "archive", "put", "--label", "SAME", "--tape", tape, "--catalog", cat, "--no-mount-check", src)
	}
	if code, _, errOut := runCmd(t, "catalog", "retire", "--catalog", cat, "SAME"); code != exitFailure || !strings.Contains(errOut, "use the tape ID") {
		t.Fatalf("%d %s", code, errOut)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestCLICopiesFlagBounds(t *testing.T) {
	for _, args := range [][]string{
		{"archive", "put", "--copies", "0", "x"},
		{"archive", "purge-source", "--copies", "-1", "x"},
	} {
		if code, _, errOut := runCmd(t, args...); code != exitUsage || !strings.Contains(errOut, "at least 1") {
			t.Fatalf("%v: %d %s", args, code, errOut)
		}
	}
}
