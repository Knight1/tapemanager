package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/Knight1/tapemanager/internal/manifest"
)

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// maxKeyFile bounds key and recipient files read from disk.
const maxKeyFile = 1 << 20

func readKeyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxKeyFile {
		return nil, fmt.Errorf("%s is too large for a key file", path)
	}
	return b, nil
}

// parseRecipients collects age recipients from --encrypt-to values and
// recipient files (one public key per line, # comments allowed).
func parseRecipients(keys []string, files []string) ([]age.Recipient, error) {
	var out []age.Recipient
	for _, k := range keys {
		rs, err := age.ParseRecipients(strings.NewReader(k))
		if err != nil {
			return nil, fmt.Errorf("--encrypt-to %q: %w", k, err)
		}
		out = append(out, rs...)
	}
	for _, f := range files {
		b, err := readKeyFile(f)
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(b), "AGE-SECRET-KEY-") {
			return nil, fmt.Errorf("%s contains a private key; pass the public key (age1...) instead, and keep the private key away from this machine", f)
		}
		rs, err := age.ParseRecipients(strings.NewReader(string(b)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, rs...)
	}
	return out, nil
}

// parseIdentities reads age private keys from files.
func parseIdentities(files []string) ([]age.Identity, error) {
	var out []age.Identity
	for _, f := range files {
		b, err := readKeyFile(f)
		if err != nil {
			return nil, err
		}
		ids, err := age.ParseIdentities(strings.NewReader(string(b)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, ids...)
	}
	return out, nil
}

// cmdArchiveKeygen creates an age key pair for encrypting files.
func cmdArchiveKeygen(args []string, stdout, stderr io.Writer) int {
	fs, _ := newFlagSet("archive keygen", stderr)
	out := fs.String("out", "", "file for the new private key (must not exist)")
	pq := fs.Bool("post-quantum", false, "create a post-quantum (ML-KEM-768 + X25519) key; needs age 1.3 or newer to decrypt")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *out == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: tapemgr archive keygen --out FILE [--post-quantum]")
		return exitUsage
	}
	var secret, public string
	if *pq {
		id, err := age.GenerateHybridIdentity()
		if err != nil {
			return fail(stderr, err)
		}
		secret, public = id.String(), id.Recipient().String()
	} else {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			return fail(stderr, err)
		}
		secret, public = id.String(), id.Recipient().String()
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		return fail(stderr, err)
	}
	// Never overwrite an existing key: that would lose access to every
	// file encrypted with it.
	if _, err := os.Lstat(*out); err == nil {
		return fail(stderr, fmt.Errorf("%s already exists; choose another file", *out))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(stderr, err)
	}
	data := fmt.Sprintf("# created by tapemgr\n# public key: %s\n%s\n", public, secret)
	if err := manifest.WriteFileAtomicPerm(*out, []byte(data), 0o600); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, `Private key written to %[1]s.
Public key: %[2]s

Encrypt with the public key (the archiving machine needs nothing else):
  tapemgr archive put --encrypt-to %[2]s <folder>
Restore with the private key:
  tapemgr archive restore --identity %[1]s --to <dir>
Without tapemgr:  age -d -i %[1]s FILE.age > FILE

Keep %[1]s away from the archiving machine and back it up. Without it,
files encrypted to this key can never be read again.
`, *out, public)
	return exitOK
}
