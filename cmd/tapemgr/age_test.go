package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var publicKeyLine = regexp.MustCompile(`Public key: (age1[0-9a-z]+)`)

func keygen(t *testing.T, out string, extra ...string) string {
	t.Helper()
	code, stdout, errOut := runCmd(t, append([]string{"archive", "keygen", "--out", out}, extra...)...)
	if code != 0 {
		t.Fatalf("keygen: %s", errOut)
	}
	m := publicKeyLine.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("no public key in %s", stdout)
	}
	if strings.Contains(stdout, "AGE-SECRET-KEY") {
		t.Fatal("private key printed")
	}
	return m[1]
}

func TestCLIAgeEncryption(t *testing.T) {
	base := t.TempDir()
	src, tape, cat := filepath.Join(base, "src"), filepath.Join(base, "tape"), filepath.Join(base, "cat")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(tape, 0o755)
	os.WriteFile(filepath.Join(src, "secret.txt"), []byte("top secret"), 0o644)
	key := filepath.Join(base, "keys", "me.txt")
	pub := keygen(t, key)
	if st, _ := os.Stat(key); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o", st.Mode().Perm())
	}
	common := []string{"--tape", tape, "--catalog", cat, "--no-mount-check"}

	if code, _, errOut := runCmd(t, append(append([]string{"archive", "put", "--encrypt-to", pub}, common...), src)...); code != 0 {
		t.Fatalf("put: %s", errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(tape, "src", "secret.txt.age")); len(b) == 0 || strings.Contains(string(b), "top secret") {
		t.Fatal("not encrypted on tape")
	}
	if code, out, _ := runCmd(t, append([]string{"archive", "list"}, common...)...); code != 0 || !strings.Contains(out, "src/secret.txt  (encrypted)") {
		t.Fatalf("list: %s", out)
	}
	if code, out, _ := runCmd(t, append([]string{"archive", "verify"}, common...)...); code != 0 || !strings.Contains(out, "successful") {
		t.Fatalf("verify without key: %d %s", code, out)
	}

	if code, out, _ := runCmd(t, append([]string{"archive", "restore", "--to", filepath.Join(base, "r1")}, common...)...); code != exitFailure || !strings.Contains(out, "pass --identity") {
		t.Fatalf("restore without key: %d %s", code, out)
	}
	dest := filepath.Join(base, "r2")
	if code, out, _ := runCmd(t, append([]string{"archive", "restore", "--identity", key, "--to", dest}, common...)...); code != 0 {
		t.Fatalf("restore: %d %s", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "src", "secret.txt")); string(b) != "top secret" {
		t.Fatalf("restored %q", b)
	}
	t.Setenv("TAPEMGR_IDENTITY", key)
	if code, out, _ := runCmd(t, append([]string{"archive", "restore", "--to", filepath.Join(base, "r3")}, common...)...); code != 0 {
		t.Fatalf("restore with $TAPEMGR_IDENTITY: %d %s", code, out)
	}
}

func TestCLIAgeRecipientsAndKeys(t *testing.T) {
	base := t.TempDir()
	src, tape := filepath.Join(base, "src"), filepath.Join(base, "tape")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(tape, 0o755)
	os.WriteFile(filepath.Join(src, "f"), []byte("data"), 0o644)
	common := []string{"--tape", tape, "--catalog", filepath.Join(base, "cat"), "--no-mount-check"}
	put := func(extra ...string) (int, string) {
		code, _, errOut := runCmd(t, append(append(append([]string{"archive", "put"}, extra...), common...), src)...)
		return code, errOut
	}

	key := filepath.Join(base, "k")
	pub := keygen(t, key)
	// A file holding the private key is refused as a recipients file.
	if code, errOut := put("--encrypt-to-file", key); code != exitFailure || !strings.Contains(errOut, "contains a private key") {
		t.Fatalf("%d %s", code, errOut)
	}
	if code, errOut := put("--encrypt-to", "age1notakey"); code != exitFailure || !strings.Contains(errOut, "--encrypt-to") {
		t.Fatalf("%d %s", code, errOut)
	}
	// Overwriting a key is refused.
	if code, _, errOut := runCmd(t, "archive", "keygen", "--out", key); code != exitFailure || !strings.Contains(errOut, "already exists") {
		t.Fatalf("%d %s", code, errOut)
	}
	if code, _, _ := runCmd(t, "archive", "keygen"); code != exitUsage {
		t.Fatal("missing --out accepted")
	}

	// A recipients file with comments and two keys, one post-quantum key
	// on its own (age does not allow mixing them).
	key2 := filepath.Join(base, "k2")
	pub2 := keygen(t, key2)
	rcpt := filepath.Join(base, "recipients")
	os.WriteFile(rcpt, []byte("# team\n"+pub+"\n"+pub2+"\n"), 0o644)
	if code, errOut := put("--encrypt-to-file", rcpt); code != 0 {
		t.Fatalf("recipients file: %s", errOut)
	}
	for _, k := range []string{key, key2} {
		dest := filepath.Join(base, "out-"+filepath.Base(k))
		if code, out, _ := runCmd(t, append([]string{"archive", "restore", "--identity", k, "--to", dest}, common...)...); code != 0 {
			t.Fatalf("restore with %s: %s", k, out)
		}
	}

	pqKey := filepath.Join(base, "pq")
	code, out, errOut := runCmd(t, "archive", "keygen", "--post-quantum", "--out", pqKey)
	m := regexp.MustCompile(`Public key: (age1pq1[0-9a-z]+)`).FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("pq keygen: %d %s %s", code, out, errOut)
	}
	os.WriteFile(filepath.Join(src, "g"), []byte("more"), 0o644)
	if code, errOut := put("--encrypt-to", m[1]); code != 0 {
		t.Fatalf("pq put: %s", errOut)
	}
	if code, out, _ := runCmd(t, append(append([]string{"archive", "restore", "--identity", pqKey, "--to", filepath.Join(base, "pq-out")}, common...), "src/g")...); code != 0 {
		t.Fatalf("pq restore: %s", out)
	}
	if code, errOut := put("--encrypt-to", m[1], "--encrypt-to", pub); code != exitFailure {
		t.Fatalf("mixed recipients accepted: %s", errOut)
	}
	if code, _, errOut := runCmd(t, append([]string{"archive", "restore", "--identity", filepath.Join(base, "missing"), "--to", base}, common...)...); code != exitFailure {
		t.Fatalf("missing identity: %s", errOut)
	}
}
