// Package ibmfw checks IBM tape drive firmware images (the "IBMTpDrv"
// container used by LTO drives) the way the drive does before it accepts
// one: container fields, the word sum and CRC of every section, the word sum
// and CRC of the whole image, and the two RSA signatures.
//
// The image is untrusted input: every offset and length read from it is
// checked against the file before use, and nothing is allocated based on
// those fields.
package ibmfw

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Record tags. Single-bit tags are checksums and signatures; the multi-bit
// ones are exclusion bitmaps.
const (
	TagSum       byte = 0x80 // 32-bit big-endian word sum
	TagCRC       byte = 0x40 // table-driven 32-bit CRC
	TagSigSHA1   byte = 0x20 // RSA PKCS#1 v1.5 signature over SHA-1
	TagSigSHA256 byte = 0x10 // RSA PKCS#1 v1.5 signature over SHA-256
)

// Magic is the container magic at offset 0x20.
const Magic = "IBMTpDrv"

const (
	headerSize = 0x50
	entrySize  = 32
	attrEntry  = 12
	bitmapSize = 32
	// sigFallbackStart: the SHA-1 signature may also cover the image from
	// here, so the first header bytes can change without re-signing.
	sigFallbackStart = 0x20
)

// ErrNotImage means the data is not an IBMTpDrv container at all.
var ErrNotImage = errors.New("not an IBM tape drive firmware image")

// Key verifies the signature records of one tag.
type Key struct {
	Name     string // certificate common name
	Hash     crypto.Hash
	Pub      *rsa.PublicKey
	NotAfter time.Time // end of the certificate's validity; the drive ignores it
	// Fingerprint is the SHA-256 of the certificate, in hex.
	Fingerprint string
}

// Keys maps a signature tag to the key that verifies it.
type Keys map[byte]Key

// Header holds the fixed fields at the start of an image.
type Header struct {
	Length        uint32
	LoadID        [4]byte
	Level         string
	Version       uint32
	HWMin, HWMax  uint32 // compatible hardware ID range
	TableOffset   uint32
	TableSize     uint32
	AttrOffset    uint32
	AttrIndexSize uint32
}

// Attr is one entry of the attribute record area.
type Attr struct {
	ID    uint32
	Value string // printable characters only
}

// Status is the result of one check.
type Status int

const (
	Absent Status = iota
	OK
	Bad
)

func (s Status) String() string {
	switch s {
	case OK:
		return "OK"
	case Bad:
		return "MISMATCH"
	}
	return "-"
}

// Section is one entry of the section table and the result of its checks.
type Section struct {
	Name                 string
	Flags                uint32
	Offset, Size         uint32
	LoadAddr, MemSize    uint32
	Required             byte // checksum tags the flags demand
	Sum, CRC             Status
	Problem              string
	TrailerOffset, Ended int // where the trailer starts and ends
}

// Record is one entry of the file-level chain, walked from the end of the
// image towards its start.
type Record struct {
	Tag        byte
	Desc       int // offset of the descriptor
	Value      int // offset of the value
	Len        int
	Status     Status
	Covered    [2]int // range the checksum or signature covers
	KeyName    string
	KeyExpired bool
	Fallback   bool  // the SHA-1 signature matched only from offset 0x20
	Denied     []int // set bits of an exclusion bitmap
	Detail     string
}

// Report is everything Inspect found. The image passes only when Problems
// is empty.
type Report struct {
	Header   Header
	Attrs    []Attr
	Platform string // attribute 9, the interface and form factor (sas_hh)
	Sections []Section
	Records  []Record
	// Signed is the end of the longest range a valid signature covers;
	// bytes from there to the end of the image are not authenticated.
	Signed   int
	Problems []string
}

// Valid reports whether every check passed.
func (r *Report) Valid() bool { return len(r.Problems) == 0 }

func (r *Report) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

func u32(b []byte, off int) uint32 { return binary.BigEndian.Uint32(b[off:]) }

// Inspect parses and checks image b. keys verifies the signatures; a
// signature without a key counts as a problem. Only data that is not an
// IBMTpDrv container at all returns an error; every other fault is listed
// in the report.
func Inspect(b []byte, keys Keys, now time.Time) (*Report, error) {
	if len(b) < headerSize || string(b[0x20:0x28]) != Magic {
		return nil, ErrNotImage
	}
	r := &Report{}
	h := &r.Header
	h.Length = u32(b, 0x04)
	copy(h.LoadID[:], b[0x08:0x0c])
	h.Level = printable(b[0x0c:0x10])
	h.Version = u32(b, 0x28)
	h.HWMin, h.HWMax = u32(b, 0x30), u32(b, 0x34)
	h.TableOffset, h.TableSize = u32(b, 0x38), u32(b, 0x3c)
	h.AttrOffset, h.AttrIndexSize = u32(b, 0x40), u32(b, 0x44)

	if len(b)%4 != 0 {
		r.problem("the image length (%d bytes) is not a multiple of 4", len(b))
	}
	if int64(h.Length) != int64(len(b)) {
		r.problem("the header says %d bytes but the image has %d: truncated or damaged", h.Length, len(b))
	}
	if h.HWMin > h.HWMax {
		r.problem("the hardware ID range is empty (%#x to %#x)", h.HWMin, h.HWMax)
	}
	r.attrs(b)
	r.sections(b)
	r.chain(b, keys, now)
	return r, nil
}

// inFile reports whether [off, off+n) lies inside b, without overflow.
func inFile(b []byte, off, n uint32) bool {
	return uint64(off)+uint64(n) <= uint64(len(b))
}

func (r *Report) attrs(b []byte) {
	h := r.Header
	if !inFile(b, h.AttrOffset, h.AttrIndexSize) {
		r.problem("the attribute area (%#x, %d bytes) lies outside the image", h.AttrOffset, h.AttrIndexSize)
		return
	}
	base := int(h.AttrOffset)
	for i := 0; i+attrEntry <= int(h.AttrIndexSize); i += attrEntry {
		e := base + i
		id, off, n := u32(b, e), u32(b, e+4), u32(b, e+8)
		if id == 0 && off == 0 && n == 0 {
			break
		}
		if uint64(off)+uint64(n) > uint64(len(b)-base) {
			r.problem("attribute %d (%#x, %d bytes) lies outside the image", id, off, n)
			continue
		}
		v := printable(b[base+int(off) : base+int(off)+int(n)])
		r.Attrs = append(r.Attrs, Attr{ID: id, Value: v})
		if id == 9 {
			r.Platform = v
		}
	}
	if r.Platform == "" {
		r.problem("the image does not name the interface it is for (attribute 9)")
	}
}

func (r *Report) sections(b []byte) {
	h := r.Header
	if !inFile(b, h.TableOffset, h.TableSize) || h.TableSize%entrySize != 0 {
		r.problem("the section table (%#x, %d bytes) is damaged", h.TableOffset, h.TableSize)
		return
	}
	for i := 0; i < int(h.TableSize)/entrySize; i++ {
		e := int(h.TableOffset) + i*entrySize
		if bytes.Equal(b[e:e+4], []byte{0, 0, 0, 0}) {
			continue
		}
		s := Section{
			Name:     printable(b[e : e+4]),
			Flags:    u32(b, e+4),
			Offset:   u32(b, e+8),
			Size:     u32(b, e+12),
			LoadAddr: u32(b, e+16),
			MemSize:  u32(b, e+20),
		}
		s.Required = byte(s.Flags >> 24)
		if s.Required != 0 {
			s.check(b)
			if s.Problem != "" {
				r.problem("section %s: %s", s.Name, s.Problem)
			}
		}
		r.Sections = append(r.Sections, s)
	}
	if len(r.Sections) == 0 {
		r.problem("the section table is empty")
	}
}

// check verifies the section's trailer: a forward list of descriptor and
// value pairs ending with a zero word, carrying exactly the checksums the
// flags demand, computed over the section's own bytes.
func (s *Section) check(b []byte) {
	if !inFile(b, s.Offset, s.Size) {
		s.Problem = fmt.Sprintf("its data (%#x, %d bytes) lies outside the image", s.Offset, s.Size)
		return
	}
	data := b[s.Offset : s.Offset+s.Size]
	p := int(s.Offset + s.Size)
	s.TrailerOffset = p
	var seen byte
	var problems []string
	for {
		if p+4 > len(b) {
			problems = append(problems, "its checksum trailer runs past the end of the image")
			break
		}
		desc := u32(b, p)
		if desc == 0 {
			p += 4
			break
		}
		tag, n, v := byte(desc>>24), int(desc&0xfff), p+4
		if n%4 != 0 || v+n > len(b) {
			problems = append(problems, fmt.Sprintf("its trailer record %#02x is damaged", tag))
			break
		}
		if singleBit(tag) {
			seen |= tag
		}
		switch {
		case tag == TagSum && n == 4:
			s.Sum = result(wordSum(data) == u32(b, v))
		case tag == TagCRC && n == 4:
			s.CRC = result(CRC(data) == u32(b, v))
		}
		p = v + n
	}
	s.Ended = p
	if s.Sum == Bad {
		problems = append(problems, "word sum mismatch")
	}
	if s.CRC == Bad {
		problems = append(problems, "CRC mismatch")
	}
	if seen != s.Required {
		problems = append(problems, fmt.Sprintf("it carries checksums %#02x, its flags demand %#02x", seen, s.Required))
	}
	s.Problem = strings.Join(problems, "; ")
}

// chain walks the file-level records backward from the end of the image.
// Each value sits right before its descriptor; checksums and signatures
// cover the image from offset 0 up to where their value is stored.
func (r *Report) chain(b []byte, keys Keys, now time.Time) {
	var seen byte
	ended := false
	off := len(b) - 4
	for off >= 4 {
		desc := u32(b, off)
		if desc == 0 {
			ended = true
			break
		}
		rec := Record{Tag: byte(desc >> 24), Desc: off, Len: int(desc & 0xfff)}
		rec.Value = off - rec.Len
		if rec.Len%4 != 0 || rec.Value < 0 {
			r.problem("record %#02x at %#x is damaged", rec.Tag, off)
			r.Records = append(r.Records, rec)
			break
		}
		if singleBit(rec.Tag) {
			seen |= rec.Tag
		}
		rec.Covered = [2]int{0, rec.Value}
		val := b[rec.Value:off]
		switch rec.Tag {
		case TagSum, TagCRC:
			if rec.Len != 4 {
				rec.Status = Bad
				rec.Detail = "wrong length"
				break
			}
			want := u32(val, 0)
			if rec.Tag == TagSum {
				rec.Status = result(wordSum(b[:rec.Value]) == want)
			} else {
				rec.Status = result(CRC(b[:rec.Value]) == want)
			}
		case TagSigSHA1, TagSigSHA256:
			r.signature(b, &rec, keys, now)
		case 0xf0, 0xf1, 0x90, 0x91:
			rec.Covered = [2]int{}
			if rec.Len < bitmapSize {
				rec.Status = Bad
				rec.Detail = "shorter than 32 bytes"
				break
			}
			rec.Status = OK
			for i, c := range val[:bitmapSize] {
				for bit := range 8 {
					if c&(0x80>>bit) != 0 {
						rec.Denied = append(rec.Denied, i*8+bit)
					}
				}
			}
		default:
			rec.Covered = [2]int{}
			rec.Status = Bad
			rec.Detail = "unknown record type"
		}
		if rec.Status == Bad {
			r.problem("%s: %s", rec.Name(), orDefault(rec.Detail, "mismatch"))
		}
		r.Records = append(r.Records, rec)
		off = rec.Value - 4
	}
	if !ended {
		r.problem("the record chain at the end of the image has no end marker")
	}
	if seen&TagCRC == 0 || seen&TagSigSHA256 == 0 {
		r.problem("the image lacks a mandatory record (CRC and SHA-256 signature are both required)")
	}
}

func (r *Report) signature(b []byte, rec *Record, keys Keys, now time.Time) {
	k, ok := keys[rec.Tag]
	if !ok || k.Pub == nil {
		rec.Status = Bad
		rec.Detail = "no key to verify it"
		return
	}
	rec.KeyName = k.Name
	rec.KeyExpired = !k.NotAfter.IsZero() && now.After(k.NotAfter)
	sig := b[rec.Value:rec.Desc]
	verify := func(data []byte) bool {
		h := k.Hash.New()
		h.Write(data)
		return rsa.VerifyPKCS1v15(k.Pub, k.Hash, h.Sum(nil), sig) == nil
	}
	switch {
	case verify(b[:rec.Value]):
		rec.Status = OK
	case rec.Tag == TagSigSHA1 && rec.Value >= sigFallbackStart && verify(b[sigFallbackStart:rec.Value]):
		rec.Status, rec.Fallback = OK, true
		rec.Covered[0] = sigFallbackStart
	default:
		rec.Status = Bad
		rec.Detail = "does not verify with " + k.Name
		return
	}
	r.Signed = max(r.Signed, rec.Value)
}

// Name describes the record type.
func (rec Record) Name() string {
	switch rec.Tag {
	case TagSum:
		return "image word sum"
	case TagCRC:
		return "image CRC"
	case TagSigSHA1:
		return "SHA-1 signature"
	case TagSigSHA256:
		return "SHA-256 signature"
	case 0xf0, 0x90:
		return "exclusion list A"
	case 0xf1, 0x91:
		return "exclusion list B"
	}
	return fmt.Sprintf("record %#02x", rec.Tag)
}

// PlatformMatches compares the image's attribute 9 with what the drive
// reports, the way the drive does: byte by byte over at most 12 bytes,
// accepting the rest once a second underscore is reached.
func PlatformMatches(image, drive string) bool {
	a, d := []byte(image), []byte(drive)
	underscores := 0
	for i := range 12 {
		var x, y byte
		if i < len(a) {
			x = a[i]
		}
		if i < len(d) {
			y = d[i]
		}
		if x == '_' {
			underscores++
			if underscores == 2 {
				return true
			}
		}
		if x != y {
			return false
		}
		if x == 0 {
			return true
		}
	}
	return true
}

func singleBit(t byte) bool { return t != 0 && t&(t-1) == 0 }

func result(ok bool) Status {
	if ok {
		return OK
	}
	return Bad
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// printable keeps letters, digits, punctuation and spaces and trims the
// result, so text from the image cannot carry terminal controls.
func printable(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			sb.WriteByte(c)
		}
	}
	return strings.TrimSpace(sb.String())
}

// wordSum adds b as 32-bit big-endian words; a partial last word is
// ignored.
func wordSum(b []byte) uint32 {
	var s uint32
	for i := 0; i+4 <= len(b); i += 4 {
		s += u32(b, i)
	}
	return s
}

var crcTable = func() (t [256]uint32) {
	for i := range t {
		a, c := uint32(gmul(byte(i), 0x38)), uint32(gmul(byte(i), 0xcf))
		t[i] = a<<24 | c<<16 | a<<8 | uint32(i)
	}
	return t
}()

// gmul multiplies in GF(2^8) with the reduction polynomial 0x11d.
func gmul(a, b byte) byte {
	var r byte
	for b != 0 {
		if b&1 != 0 {
			r ^= a
		}
		hi := a & 0x80
		a <<= 1
		if hi != 0 {
			a ^= 0x1d
		}
		b >>= 1
	}
	return r
}

// CRC is the drive's byte-wise, most significant first table CRC.
func CRC(b []byte) uint32 {
	var c uint32
	for _, x := range b {
		c = crcTable[byte(c>>24)^x] ^ c<<8
	}
	return c
}
