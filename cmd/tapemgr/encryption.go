package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/drive"
	"github.com/Knight1/tapemanager/internal/manifest"
)

// driveEncryption returns the encryption setting of the drive holding the
// tape mounted at tapeRoot, or an error saying why it is unknown.
var driveEncryption = realDriveEncryption

func realDriveEncryption(tapeRoot string) (*drive.EncryptionStatus, error) {
	f, err := forMount(tapeRoot)
	if err != nil {
		return nil, err
	}
	d, err := openDevice(f.SG)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return drive.ReadEncryptionStatus(d)
}

// checkEncryption decides whether writing to the tape at tapeRoot may go
// ahead. A tape that was written encrypted never gets plain data added,
// and with require set the drive must be encrypting. It returns the
// drive's setting if known, so the caller can record it after writing.
func checkEncryption(cat *catalog.Catalog, tapeRoot string, require bool, log io.Writer) (*drive.EncryptionStatus, error) {
	st, serr := driveEncryption(tapeRoot)

	var rec *catalog.Tape
	if tp, err := manifest.Open(tapeRoot); err == nil {
		if v, err := tp.Volume(); err == nil && v != nil {
			rec, _ = cat.Tape(v.ID)
		}
		tp.Close()
	}
	if rec != nil && rec.Encryption != nil {
		name := tapeName(&rec.Volume)
		if serr != nil {
			return nil, fmt.Errorf("tape %s holds encrypted data, but the drive's encryption setting cannot be read (%v); refusing to add unencrypted data", name, serr)
		}
		if !st.Encrypting() {
			return nil, fmt.Errorf("tape %s holds encrypted data, but the drive is not encrypting; mount the tape with its LTFS key (see docs/TECHNICAL.md); refusing to add unencrypted data", name)
		}
		if k := st.KeyName(); k != "" && len(rec.Encryption.Keys) > 0 && !containsString(rec.Encryption.Keys, k) {
			fmt.Fprintf(log, "NOTE:      tape %s was written with key %s; new files are encrypted with key %s, so both keys are needed to read the tape\n",
				name, strings.Join(rec.Encryption.Keys, ", "), k)
		}
	}
	if require {
		if serr != nil {
			return nil, fmt.Errorf("--require-encryption: cannot read the drive's encryption setting: %v", serr)
		}
		if !st.Encrypting() {
			return nil, errors.New("--require-encryption: the drive is not encrypting; format and mount the tape with an LTFS key (see docs/TECHNICAL.md)")
		}
	}
	if serr != nil {
		return nil, nil
	}
	return st, nil
}

// recordEncryption notes in the catalog that tape id was written while the
// drive was encrypting.
func recordEncryption(cat *catalog.Catalog, id string, st *drive.EncryptionStatus, stderr io.Writer) {
	if st == nil || !st.Encrypting() || id == "" {
		return
	}
	if err := cat.RecordDriveEncryption(id, st.KeyName()); err != nil {
		fmt.Fprintf(stderr, "tapemgr: recording encryption of tape %s in the catalog: %v\n", id, err)
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var keyPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9]{3}$`)

// cmdDriveKeygen adds a new data key to an LTFS key file (the flatfile key
// manager format: alternating DK= and DKi= lines).
func cmdDriveKeygen(args []string, stdout, stderr io.Writer) int {
	fs, _ := newFlagSet("drive keygen", stderr)
	out := fs.String("out", "", "new LTFS key file (an existing one only with --append)")
	prefix := fs.String("prefix", "TMG", "three letters or digits that start the key ID")
	appendKey := fs.Bool("append", false, "add the key to an existing key file instead of refusing it")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *out == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: tapemgr drive keygen --out FILE [--append] [--prefix ABC]")
		return exitUsage
	}
	if !keyPrefixPattern.MatchString(*prefix) {
		fmt.Fprintln(stderr, "tapemgr: --prefix must be exactly three letters or digits")
		return exitUsage
	}

	var old []byte
	st, err := os.Lstat(*out)
	switch {
	case err == nil && !*appendKey:
		return fail(stderr, fmt.Errorf("%s already exists; pass --append to add a new key to it", *out))
	case err == nil:
		if !st.Mode().IsRegular() {
			return fail(stderr, fmt.Errorf("%s is not a regular file", *out))
		}
		if st.Mode().Perm()&0o077 != 0 {
			return fail(stderr, fmt.Errorf("%s is readable or writable by other users (mode %o); run 'chmod 600 %s' first", *out, st.Mode().Perm(), *out))
		}
		if old, err = os.ReadFile(*out); err != nil {
			return fail(stderr, err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
			return fail(stderr, err)
		}
	default:
		return fail(stderr, err)
	}

	var key [32]byte
	var id [9]byte
	if _, err := rand.Read(key[:]); err != nil {
		return fail(stderr, err)
	}
	if _, err := rand.Read(id[:]); err != nil {
		return fail(stderr, err)
	}
	dki := *prefix + hex.EncodeToString(id[:])
	if strings.Contains(string(old), "DKi="+dki+"\n") {
		return fail(stderr, errors.New("generated key ID already exists; run again"))
	}

	data := append([]byte(nil), old...)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = fmt.Appendf(data, "DK=%s\nDKi=%s\n", base64.StdEncoding.EncodeToString(key[:]), dki)
	// The old file stays in place until the new one is complete: losing
	// the key file means losing every tape encrypted with it.
	write := manifest.WriteFileAtomicPerm
	if len(old) == 0 && !*appendKey {
		// A new file: never replace one that appeared in the meantime.
		write = manifest.WriteFileNew
	}
	if err := write(*out, data, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fail(stderr, fmt.Errorf("%s already exists; pass --append to add a new key to it", *out))
		}
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, `Added key %[1]s to %[2]s.

Format a tape with it:
  mkltfs -d /dev/st0 -n LABEL --kmi-backend=flatfile -o kmi_dk_list=%[2]s -o kmi_dki_for_format=%[1]s
Mount encrypted tapes with:
  ltfs /mnt/ltfs -o devname=/dev/st0 -o kmi_backend=flatfile -o kmi_dk_list=%[2]s

Back up %[2]s now and keep a copy away from this machine. Without it,
tapes encrypted with this key can never be read again.
`, dki, *out)
	return exitOK
}
