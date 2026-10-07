// Package ibmfwtest builds IBM-style firmware images signed with test keys,
// so the image checks can be tested without vendor images.
package ibmfwtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/Knight1/tapemanager/internal/ibmfw"
)

var (
	once  sync.Once
	privs map[byte]*rsa.PrivateKey
)

func testPrivs() map[byte]*rsa.PrivateKey {
	once.Do(func() {
		privs = map[byte]*rsa.PrivateKey{}
		for _, tag := range []byte{ibmfw.TagSigSHA1, ibmfw.TagSigSHA256} {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			privs[tag] = k
		}
	})
	return privs
}

// Keys returns the public test keys that verify images from Build.
func Keys() ibmfw.Keys {
	p := testPrivs()
	return ibmfw.Keys{
		ibmfw.TagSigSHA1:   {Name: "TEST_SHA1", Hash: crypto.SHA1, Pub: &p[ibmfw.TagSigSHA1].PublicKey},
		ibmfw.TagSigSHA256: {Name: "TEST_SHA256", Hash: crypto.SHA256, Pub: &p[ibmfw.TagSigSHA256].PublicKey},
	}
}

// Spec describes an image to build. Zero fields get defaults.
type Spec struct {
	Size     int    // total size, a multiple of 4; the main section fills it
	Level    string // four characters
	LoadID   []byte // 4 bytes
	ModelID  []byte // 8 bytes, EBCDIC
	Platform string // attribute 9, default sas_hh
	Built    string // attribute 4, default 2026/01/02
	Filler   func([]byte)

	// SHA1FromHeaderEnd signs the SHA-1 record over the image from 0x20,
	// the drive's fallback range.
	SHA1FromHeaderEnd bool
	// Deny sets bits in exclusion list A.
	Deny []int
}

// Layout tells where Build put things.
type Layout struct {
	Main        [2]int // data of the main section
	MainTrailer int    // offset of its trailer
	SumValue    int
	CRCValue    int
	SHA1Value   int
	SHA256Value int
	SHA256Desc  int
	ListADesc   int
}

const (
	headerSize = 0x50
	sections   = 4
	tableSize  = sections * 32
	trailer    = 20
	chainSize  = 4 + 8 + 8 + 260 + 260 + 36 + 36
	fcodSize   = 64
)

// Build returns a signed image and its layout.
func Build(s Spec) ([]byte, Layout) {
	if s.Level == "" {
		s.Level = "TST1"
	}
	if s.Platform == "" {
		s.Platform = "sas_hh"
	}
	if s.Built == "" {
		s.Built = "2026/01/02"
	}
	if s.LoadID == nil {
		s.LoadID = []byte{0x11, 0x22, 0x33, 0x44}
	}
	if s.ModelID == nil {
		s.ModelID = []byte{0xE3, 0xC5, 0xE2, 0xE3, 0xC9, 0xC4, 0xF0, 0xF1}
	}
	attrs := []string{"tst000000a", "EC-NEW", "PART-NO", s.Built, "12:00:00", "TEST", "TEST_" + s.Level, "tst000000a", s.Platform}
	attrIndex := (len(attrs) + 1) * 12
	pool := 0
	for _, a := range attrs {
		pool += (len(a) + 3) &^ 3
	}
	attrOff := headerSize + tableSize
	fcodOff := attrOff + attrIndex + pool
	mainOff := fcodOff + fcodSize + trailer
	fixed := mainOff + trailer + chainSize
	if s.Size == 0 {
		s.Size = fixed + 4096
	}
	mainSize := s.Size - fixed
	if mainSize < 0 || s.Size%4 != 0 {
		panic(fmt.Sprintf("ibmfwtest: size %d too small or not a multiple of 4 (at least %d)", s.Size, fixed))
	}

	b := make([]byte, s.Size)
	if s.Filler != nil {
		s.Filler(b[mainOff : mainOff+mainSize])
	}
	put := func(off int, v uint32) { binary.BigEndian.PutUint32(b[off:], v) }
	put(0, 0x48000391)
	put(4, uint32(s.Size))
	copy(b[8:12], s.LoadID)
	copy(b[12:16], s.Level)
	copy(b[0x18:0x20], s.ModelID)
	copy(b[0x20:0x28], ibmfw.Magic)
	put(0x28, 0x00010001)
	put(0x2c, 0x18)
	put(0x30, 0x00010060)
	put(0x34, 0x00010061)
	put(0x38, headerSize)
	put(0x3c, tableSize)
	put(0x40, uint32(attrOff))
	put(0x44, uint32(attrIndex))
	put(0x48, 0x60000000)
	put(0x4c, 0x60000000)

	// Attributes: index, then the string pool.
	p := attrIndex
	for i, a := range attrs {
		e := attrOff + i*12
		put(e, uint32(i+1))
		put(e+4, uint32(p))
		put(e+8, uint32(len(a)))
		copy(b[attrOff+p:], a)
		p += (len(a) + 3) &^ 3
	}

	l := Layout{Main: [2]int{mainOff, mainOff + mainSize}, MainTrailer: mainOff + mainSize}
	entry := func(i int, name string, flags uint32, off, size int) {
		e := headerSize + i*32
		copy(b[e:e+4], name)
		put(e+4, flags)
		put(e+8, uint32(off))
		put(e+12, uint32(size))
		put(e+16, 0x10000000+uint32(i)<<20)
		put(e+20, uint32(size))
	}
	entry(0, "fcod", 0xc0000100, fcodOff, fcodSize)
	entry(1, "vmli", 0xc0000100, mainOff, mainSize)
	entry(2, "kmem", 0x00000200, s.Size, 0)
	// Entry 3 stays empty, as unused table slots do.
	for i := range fcodSize {
		b[fcodOff+i] = byte(i * 7)
	}
	sealSection(b, fcodOff, fcodSize)
	sealSection(b, mainOff, mainSize)

	// The file-level chain: a zero end marker, then each record's value
	// followed by its descriptor.
	c := mainOff + mainSize + trailer
	put(c, 0)
	l.SumValue = c + 4
	l.CRCValue = l.SumValue + 8
	l.SHA1Value = l.CRCValue + 8
	l.SHA256Value = l.SHA1Value + 260
	l.SHA256Desc = l.SHA256Value + 256
	listA := l.SHA256Desc + 4
	l.ListADesc = listA + 32
	listB := l.ListADesc + 4
	put(l.SumValue+4, uint32(ibmfw.TagSum)<<24|4)
	put(l.CRCValue+4, uint32(ibmfw.TagCRC)<<24|4)
	put(l.SHA1Value+256, uint32(ibmfw.TagSigSHA1)<<24|256)
	put(l.SHA256Desc, uint32(ibmfw.TagSigSHA256)<<24|256)
	for _, bit := range s.Deny {
		b[listA+bit/8] |= 0x80 >> (bit % 8)
	}
	put(l.ListADesc, 0xf0<<24|32)
	put(listB+32, 0xf1<<24|32)
	SealImage(b, l)
	sign(b, l, s.SHA1FromHeaderEnd)
	return b, l
}

// sealSection writes the word sum and CRC of the section at off.
func sealSection(b []byte, off, size int) {
	t := off + size
	binary.BigEndian.PutUint32(b[t:], uint32(ibmfw.TagSum)<<24|4)
	binary.BigEndian.PutUint32(b[t+4:], wordSum(b[off:t]))
	binary.BigEndian.PutUint32(b[t+8:], uint32(ibmfw.TagCRC)<<24|4)
	binary.BigEndian.PutUint32(b[t+12:], ibmfw.CRC(b[off:t]))
	binary.BigEndian.PutUint32(b[t+16:], 0)
}

// SealMain recomputes the main section's word sum and CRC, as someone
// changing its data would.
func SealMain(b []byte, l Layout) { sealSection(b, l.Main[0], l.Main[1]-l.Main[0]) }

// SealImage recomputes the whole-image word sum and CRC. The signatures
// stay as they are.
func SealImage(b []byte, l Layout) {
	binary.BigEndian.PutUint32(b[l.SumValue:], wordSum(b[:l.SumValue]))
	binary.BigEndian.PutUint32(b[l.CRCValue:], ibmfw.CRC(b[:l.CRCValue]))
}

func sign(b []byte, l Layout, sha1FromHeaderEnd bool) {
	p := testPrivs()
	from := 0
	if sha1FromHeaderEnd {
		from = 0x20
	}
	for _, s := range []struct {
		tag   byte
		hash  crypto.Hash
		from  int
		value int
	}{
		{ibmfw.TagSigSHA1, crypto.SHA1, from, l.SHA1Value},
		{ibmfw.TagSigSHA256, crypto.SHA256, 0, l.SHA256Value},
	} {
		h := s.hash.New()
		h.Write(b[s.from:s.value])
		sig, err := rsa.SignPKCS1v15(nil, p[s.tag], s.hash, h.Sum(nil))
		if err != nil {
			panic(err)
		}
		copy(b[s.value:], sig)
	}
}

func wordSum(b []byte) uint32 {
	var s uint32
	for i := 0; i+4 <= len(b); i += 4 {
		s += binary.BigEndian.Uint32(b[i:])
	}
	return s
}
