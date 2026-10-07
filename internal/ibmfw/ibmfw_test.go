package ibmfw_test

import (
	"bytes"
	"crypto"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Knight1/tapemanager/internal/ibmfw"
	"github.com/Knight1/tapemanager/internal/ibmfw/ibmfwtest"
)

var now = time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)

func inspect(t *testing.T, b []byte, keys ibmfw.Keys) *ibmfw.Report {
	t.Helper()
	r, err := ibmfw.Inspect(b, keys, now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func hasProblem(r *ibmfw.Report, s string) bool {
	return slices.ContainsFunc(r.Problems, func(p string) bool { return strings.Contains(p, s) })
}

func TestValidImage(t *testing.T) {
	b, l := ibmfwtest.Build(ibmfwtest.Spec{Size: 64 << 10, Level: "AB12"})
	r := inspect(t, b, ibmfwtest.Keys())
	if !r.Valid() {
		t.Fatal(r.Problems)
	}
	if r.Header.Level != "AB12" || r.Platform != "sas_hh" || r.Signed != l.SHA256Value || len(r.Records) != 6 || len(r.Sections) != 3 {
		t.Fatalf("%+v", r)
	}
	for _, s := range r.Sections {
		if s.Required != 0 && (s.Sum != ibmfw.OK || s.CRC != ibmfw.OK) {
			t.Fatalf("%+v", s)
		}
	}
	for _, rec := range r.Records {
		if rec.Status != ibmfw.OK || len(rec.Denied) != 0 {
			t.Fatalf("%+v", rec)
		}
	}
}

// Each layer must reject on its own: repairing the checksums that anyone
// can recompute still leaves the signature failing.
func TestTamperedImages(t *testing.T) {
	keys := ibmfwtest.Keys()
	b, l := ibmfwtest.Build(ibmfwtest.Spec{Size: 64 << 10})
	flip := func() []byte {
		c := bytes.Clone(b)
		c[l.Main[0]+100] ^= 1
		return c
	}

	c := flip()
	r := inspect(t, c, keys)
	if !hasProblem(r, "section vmli: word sum mismatch; CRC mismatch") || !hasProblem(r, "image CRC") || !hasProblem(r, "SHA-256 signature") {
		t.Fatalf("flipped: %v", r.Problems)
	}

	ibmfwtest.SealMain(c, l)
	r = inspect(t, c, keys)
	if hasProblem(r, "section vmli") || !hasProblem(r, "image word sum") || !hasProblem(r, "SHA-1 signature") {
		t.Fatalf("section repaired: %v", r.Problems)
	}

	ibmfwtest.SealImage(c, l)
	r = inspect(t, c, keys)
	if len(r.Problems) != 2 || !hasProblem(r, "SHA-1 signature: does not verify") || !hasProblem(r, "SHA-256 signature: does not verify") {
		t.Fatalf("image sums repaired: %v", r.Problems)
	}

	// Removing the SHA-256 record ends the chain early.
	c = bytes.Clone(b)
	binary.BigEndian.PutUint32(c[l.SHA256Desc:], 0)
	r = inspect(t, c, keys)
	if !hasProblem(r, "lacks a mandatory record") {
		t.Fatalf("signature removed: %v", r.Problems)
	}
}

// The tail after the SHA-256 signature is not signed, so changing it is
// not caught by the signatures; the report says where the signed part ends.
func TestUnsignedTail(t *testing.T) {
	b, l := ibmfwtest.Build(ibmfwtest.Spec{Size: 64 << 10, Deny: []int{24, 200}})
	r := inspect(t, b, ibmfwtest.Keys())
	if !r.Valid() || r.Signed != l.SHA256Value || len(b)-r.Signed != 256+4+36+36 {
		t.Fatalf("%v %#x", r.Problems, r.Signed)
	}
	var a ibmfw.Record
	for _, rec := range r.Records {
		if rec.Tag == 0xf0 {
			a = rec
		}
	}
	if !slices.Equal(a.Denied, []int{24, 200}) || a.Name() != "exclusion list A" {
		t.Fatalf("%+v", a)
	}
}

func TestSHA1Fallback(t *testing.T) {
	b, _ := ibmfwtest.Build(ibmfwtest.Spec{SHA1FromHeaderEnd: true})
	r := inspect(t, b, ibmfwtest.Keys())
	if !r.Valid() {
		t.Fatal(r.Problems)
	}
	for _, rec := range r.Records {
		if rec.Tag == ibmfw.TagSigSHA1 && (!rec.Fallback || rec.Covered[0] != 0x20) {
			t.Fatalf("%+v", rec)
		}
	}
}

func TestKeys(t *testing.T) {
	b, _ := ibmfwtest.Build(ibmfwtest.Spec{})
	test := ibmfwtest.Keys()

	// Keys of the wrong tag do not verify.
	swapped := ibmfw.Keys{ibmfw.TagSigSHA1: test[ibmfw.TagSigSHA256], ibmfw.TagSigSHA256: test[ibmfw.TagSigSHA1]}
	swapped[ibmfw.TagSigSHA1] = ibmfw.Key{Name: "X", Hash: crypto.SHA1, Pub: swapped[ibmfw.TagSigSHA1].Pub}
	swapped[ibmfw.TagSigSHA256] = ibmfw.Key{Name: "Y", Hash: crypto.SHA256, Pub: swapped[ibmfw.TagSigSHA256].Pub}
	if r := inspect(t, b, swapped); !hasProblem(r, "does not verify with X") || !hasProblem(r, "does not verify with Y") {
		t.Fatalf("swapped: %v", r.Problems)
	}
	// Without keys, signatures cannot pass.
	if r := inspect(t, b, nil); !hasProblem(r, "no key") {
		t.Fatalf("no keys: %v", r.Problems)
	}
	// The vendor keys reject a test image.
	ibm, err := ibmfw.IBMKeys()
	if err != nil {
		t.Fatal(err)
	}
	if r := inspect(t, b, ibm); r.Valid() {
		t.Fatal("test image passed with the vendor keys")
	}
	// An expired certificate is reported, but the drive ignores validity.
	exp := ibmfw.Keys{}
	for tag, k := range test {
		k.NotAfter = now.Add(-time.Hour)
		exp[tag] = k
	}
	r := inspect(t, b, exp)
	if !r.Valid() {
		t.Fatal(r.Problems)
	}
	for _, rec := range r.Records {
		if rec.KeyName != "" && !rec.KeyExpired {
			t.Fatalf("%+v", rec)
		}
	}
}

// The embedded certificates are IBM's; a swapped file must not go unnoticed.
func TestIBMKeys(t *testing.T) {
	keys, err := ibmfw.IBMKeys()
	if err != nil {
		t.Fatal(err)
	}
	want := map[byte]struct {
		name     string
		hash     crypto.Hash
		notAfter string
	}{
		ibmfw.TagSigSHA1:   {"L4H_FIRMWARE", crypto.SHA1, "2026-06-30"},
		ibmfw.TagSigSHA256: {"TAPEFIRMWARE", crypto.SHA256, "2030-07-02"},
	}
	for tag, w := range want {
		k := keys[tag]
		if k.Name != w.name || k.Hash != w.hash || k.Pub.N.BitLen() != 2048 || k.Pub.E != 65537 || k.NotAfter.Format(time.DateOnly) != w.notAfter {
			t.Errorf("%#x: %s %v %d %s", tag, k.Name, k.Hash, k.Pub.N.BitLen(), k.NotAfter)
		}
	}
	for tag, fp := range map[byte]string{
		ibmfw.TagSigSHA1:   "5c8d14db9e92068a832e4122f358b62ecf0b101190e4952b9afc0df298a5eef0",
		ibmfw.TagSigSHA256: "8a249bfcaa568074d937569bdcca13229585426182b66fde25143fb3b1737782",
	} {
		if keys[tag].Fingerprint != fp {
			t.Errorf("%#x: fingerprint %s", tag, keys[tag].Fingerprint)
		}
	}
}

func TestDamagedContainer(t *testing.T) {
	keys := ibmfwtest.Keys()
	b, l := ibmfwtest.Build(ibmfwtest.Spec{Size: 8 << 10})
	ao := int(binary.BigEndian.Uint32(b[0x40:])) // attribute area

	if _, err := ibmfw.Inspect(b[:0x40], keys, now); !errors.Is(err, ibmfw.ErrNotImage) {
		t.Fatal(err)
	}
	c := bytes.Clone(b)
	c[0x20] = 'X'
	if _, err := ibmfw.Inspect(c, keys, now); !errors.Is(err, ibmfw.ErrNotImage) {
		t.Fatal(err)
	}
	if r := inspect(t, b[:len(b)-1], keys); !hasProblem(r, "not a multiple of 4") || !hasProblem(r, "truncated or damaged") {
		t.Fatalf("truncated: %v", r.Problems)
	}
	for name, edit := range map[string]func([]byte){
		"section table":          func(c []byte) { binary.BigEndian.PutUint32(c[0x38:], 0xfffffff0) },
		"attribute area":         func(c []byte) { binary.BigEndian.PutUint32(c[0x40:], 0xfffffff0) },
		"lies outside":           func(c []byte) { binary.BigEndian.PutUint32(c[0x50+32+8:], 0xfffffff0) },
		"flags demand 0x80":      func(c []byte) { c[0x50+32+4] = 0x80 },
		"hardware ID range":      func(c []byte) { binary.BigEndian.PutUint32(c[0x30:], 0xffffffff) },
		"record 0x":              func(c []byte) { binary.BigEndian.PutUint32(c[len(c)-4:], 0xf1000021) },
		"unknown record":         func(c []byte) { c[len(c)-4] = 0x77 },
		"trailer":                func(c []byte) { binary.BigEndian.PutUint32(c[l.MainTrailer:], 0x80000fff) },
		"attribute 9":            func(c []byte) { binary.BigEndian.PutUint32(c[ao+8*12:], 77) },
		"attribute 3 (0x":        func(c []byte) { binary.BigEndian.PutUint32(c[ao+2*12+4:], 0xfffffff0) },
		"shorter than 32":        func(c []byte) { binary.BigEndian.PutUint32(c[len(c)-4:], 0xf1000010) },
		"4095 bytes) is damaged": func(c []byte) { binary.BigEndian.PutUint32(c[0x3c:], 0xfff) },
		"section table is e":     func(c []byte) { clear(c[0x50 : 0x50+4*32]) },
	} {
		c := bytes.Clone(b)
		edit(c)
		r := inspect(t, c, keys)
		if !hasProblem(r, name) {
			t.Errorf("%s: %v", name, r.Problems)
		}
	}
}

// A chain whose records lead back to the start of the image without a
// zero end marker is refused.
func TestChainWithoutEnd(t *testing.T) {
	c := make([]byte, 0x60)
	copy(c[0x20:], ibmfw.Magic)
	binary.BigEndian.PutUint32(c[len(c)-4:], 0xf1000000|uint32(len(c)-4))
	if r := inspect(t, c, nil); !hasProblem(r, "no end marker") {
		t.Fatal(r.Problems)
	}
}

func TestPlatformMatches(t *testing.T) {
	for _, c := range []struct {
		image, drive string
		ok           bool
	}{
		{"sas_hh", "sas_hh", true},
		{"sas_hh", "fc_hh", false},
		{"sas_hh", "sas_fh", false},
		{"sas_hh", "sas", false},
		{"sas_hh_a", "sas_hh_b", true}, // the second underscore ends the comparison
		{"sas_hh", "sas_hh_x", false},
		{"abcdefghijklX", "abcdefghijklY", true}, // only 12 bytes count
		{"", "", true},
	} {
		if got := ibmfw.PlatformMatches(c.image, c.drive); got != c.ok {
			t.Errorf("%q %q: %v", c.image, c.drive, got)
		}
	}
}

func TestCRC(t *testing.T) {
	if ibmfw.CRC(nil) != 0 {
		t.Fatal("empty")
	}
	// The table has an identity low byte, so one byte b yields table[b].
	if c := ibmfw.CRC([]byte{1}); c&0xff != 1 || c>>24 != (c>>8)&0xff {
		t.Fatalf("%#x", c)
	}
	// GF(2)-linear: crc(a xor b) == crc(a) xor crc(b) for equal lengths.
	a, b := []byte("firmware image A"), []byte("another block 16")
	x := make([]byte, len(a))
	for i := range a {
		x[i] = a[i] ^ b[i]
	}
	if ibmfw.CRC(x) != ibmfw.CRC(a)^ibmfw.CRC(b) {
		t.Fatal("not linear")
	}
}

// A valid image must stop passing when any signed byte changes.
func FuzzSignedBytes(f *testing.F) {
	b, _ := ibmfwtest.Build(ibmfwtest.Spec{Size: 4 << 10})
	keys := ibmfwtest.Keys()
	base, err := ibmfw.Inspect(b, keys, now)
	if err != nil || !base.Valid() {
		f.Fatal(err, base.Problems)
	}
	f.Add(0, byte(1))
	f.Add(0x30, byte(0x80))
	f.Add(len(b)-1, byte(1))
	f.Fuzz(func(t *testing.T, off int, x byte) {
		if off < 0 || off >= len(b) || x == 0 {
			return
		}
		c := bytes.Clone(b)
		c[off] ^= x
		r, err := ibmfw.Inspect(c, keys, now)
		if err != nil {
			if off < 0x20 || off >= 0x28 {
				t.Fatalf("offset %#x: %v", off, err)
			}
			return
		}
		if r.Valid() && off < base.Signed {
			t.Fatalf("change at %#x inside the signed range passed", off)
		}
	})
}

// Arbitrary data must never panic or pass.
func FuzzInspect(f *testing.F) {
	b, _ := ibmfwtest.Build(ibmfwtest.Spec{Size: 2 << 10})
	keys := ibmfwtest.Keys()
	f.Add(b)
	f.Add(b[:0x60])
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := ibmfw.Inspect(data, keys, now)
		if err != nil {
			return
		}
		if r.Valid() && (r.Signed > len(b) || !bytes.Equal(data[:r.Signed], b[:r.Signed])) {
			t.Fatal("unsigned data passed")
		}
	})
}
