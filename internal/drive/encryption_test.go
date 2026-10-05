package drive

import (
	"errors"
	"testing"
	"time"
)

// Data encryption capabilities as an IBM ULT3580-HH6 reports them with an
// LTO-6 cartridge (no identifiers in this page).
const sampleEncCapabilities = `00 10 00 58 09 00 00 00 00 00 00 00 00 00 00 00
	00 00 00 00 01 00 00 14 3a 34 00 20 00 0c 00 20 eb 00 00 00 00 00 00 00
	00 01 00 14 02 00 00 14 3a 3c 00 20 00 3c 00 20 eb 00 00 00 00 00 00 00
	00 01 00 14 03 00 00 14 ba 3c 00 20 00 3c 00 20 eb 00 00 00 00 00 00 00
	00 01 00 14`

func TestParseAlgorithmsReal(t *testing.T) {
	algs := parseAlgorithms(unhex(t, sampleEncCapabilities))
	if len(algs) != 3 {
		t.Fatalf("%+v", algs)
	}
	for i, a := range algs {
		if a.Index != byte(i+1) || a.Name() != "AES-256-GCM" || a.KeyBits != 256 {
			t.Fatalf("%+v", a)
		}
		// Only the third is valid for the mounted cartridge.
		if a.Usable != (i == 2) {
			t.Fatalf("usable %d: %+v", i, a)
		}
	}
	if (Algorithm{Code: 0x1234}).Name() != "algorithm 0x00001234" {
		t.Fatal("unknown algorithm name")
	}
	// Torn or undersized descriptors stop parsing.
	b := unhex(t, sampleEncCapabilities)
	if got := parseAlgorithms(b[:50]); len(got) != 1 {
		t.Fatalf("torn: %+v", got)
	}
	b[23] = 4 // descriptor shorter than its fixed fields
	if got := parseAlgorithms(b); len(got) != 0 {
		t.Fatalf("short descriptor: %+v", got)
	}
}

func TestParseEncryptionStatus(t *testing.T) {
	off := make([]byte, 24)
	off[1], off[3] = 0x20, 20
	s, err := parseEncryptionStatus(off)
	if err != nil || s.Encrypting() || s.Decrypting() || s.KeyName() != "" {
		t.Fatalf("%+v %v", s, err)
	}
	on := append(append([]byte(nil), off...), 1, 0, 0, 12)
	on = append(on, 'T', 'M', 'G', 1, 2, 3, 4, 5, 6, 7, 8, 9)
	on[5], on[6], on[7] = 2, 3, 1
	s, err = parseEncryptionStatus(on)
	if err != nil || !s.Encrypting() || !s.Decrypting() || s.AlgorithmIndex != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	if s.KeyName() != "TMG010203040506070809" {
		t.Fatalf("%q", s.KeyName())
	}
	// A KAD claiming more bytes than received is ignored.
	torn := append(append([]byte(nil), off...), 1, 0, 0, 40, 'x')
	if s, err := parseEncryptionStatus(torn); err != nil || s.KeyID != nil {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := parseEncryptionStatus(off[:20]); !errors.Is(err, errShort) {
		t.Fatalf("%v", err)
	}
}

func TestFormatKeyID(t *testing.T) {
	cases := map[string][]byte{
		"":                         nil,
		"TMG010203040506070809":    {'T', 'M', 'G', 1, 2, 3, 4, 5, 6, 7, 8, 9},
		"backup-key":               []byte("backup-key"),
		"00ff10":                   {0, 0xff, 0x10},
		"0102030405060708090a0b0c": {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
	}
	for want, id := range cases {
		if got := FormatKeyID(id); got != want {
			t.Errorf("%x: %q, want %q", id, got, want)
		}
	}
}

type pageDevice struct {
	resp []byte
}

func (p pageDevice) Do(cdb []byte, dir Direction, buf []byte, _ time.Duration) (int, error) {
	return copy(buf, p.resp), nil
}
func (p pageDevice) Path() string { return "x" }
func (p pageDevice) Close() error { return nil }

func TestReadSecurityPageChecks(t *testing.T) {
	if _, err := ReadEncryptionStatus(pageDevice{resp: []byte{0, 0x10, 0, 0}}); err == nil {
		t.Fatal("wrong page accepted")
	}
	if _, err := ReadEncryptionStatus(pageDevice{resp: []byte{0, 0x20}}); !errors.Is(err, errShort) {
		t.Fatalf("%v", err)
	}
	if c := securityInCDB(0x20, 8192); c[0] != 0xA2 || c[1] != 0x20 || c[3] != 0x20 || c[8] != 0x20 || c[9] != 0 {
		t.Fatalf("% x", c)
	}
}

func FuzzParseEncryption(f *testing.F) {
	f.Add(unhex(f, sampleEncCapabilities))
	f.Fuzz(func(t *testing.T, b []byte) {
		parseAlgorithms(b)
		if s, err := parseEncryptionStatus(b); err == nil {
			s.KeyName()
		}
	})
}
