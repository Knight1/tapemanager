package drive

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Tape data encryption pages of SECURITY PROTOCOL IN, protocol 0x20 (SSC-4).
const (
	opSecurityIn         = 0xA2
	protoTapeEncryption  = 0x20
	pageEncCapabilities  = 0x0010
	pageEncStatus        = 0x0020
	encModeEncrypt       = 0x02
	decModeDecrypt       = 0x02
	decModeMixed         = 0x03
	kadTypeUnauth        = 0x00
	kadTypeAuth          = 0x01
	algorithmAES256GCM   = 0x00010014
	ltfsKeyIDLength      = 12
	ltfsKeyIDASCIILength = 3
)

func securityInCDB(page uint16, alloc uint32) []byte {
	cdb := make([]byte, 12)
	cdb[0] = opSecurityIn
	cdb[1] = protoTapeEncryption
	binary.BigEndian.PutUint16(cdb[2:], page)
	binary.BigEndian.PutUint32(cdb[6:], alloc)
	return cdb
}

func readSecurityPage(d Device, page uint16) ([]byte, error) {
	buf := make([]byte, 8192)
	n, err := d.Do(securityInCDB(page, uint32(len(buf))), DirIn, buf, shortTimeout)
	if err != nil {
		return nil, err
	}
	b := buf[:n]
	if len(b) < 4 {
		return nil, errShort
	}
	if got := binary.BigEndian.Uint16(b); got != page {
		return nil, fmt.Errorf("drive returned security page 0x%04x instead of 0x%04x", got, page)
	}
	end := min(4+int(binary.BigEndian.Uint16(b[2:])), len(b))
	return b[:end], nil
}

// Algorithm is an encryption algorithm the drive supports.
type Algorithm struct {
	Index   byte
	Code    uint32
	KeyBits int
	// Usable means the drive can encrypt with it in hardware and the
	// loaded cartridge supports it.
	Usable bool
}

// Name describes the algorithm.
func (a Algorithm) Name() string {
	if a.Code == algorithmAES256GCM {
		return "AES-256-GCM"
	}
	return fmt.Sprintf("algorithm 0x%08x", a.Code)
}

// ReadEncryptionAlgorithms lists the drive's encryption algorithms.
func ReadEncryptionAlgorithms(d Device) ([]Algorithm, error) {
	b, err := readSecurityPage(d, pageEncCapabilities)
	if err != nil {
		return nil, err
	}
	return parseAlgorithms(b), nil
}

func parseAlgorithms(b []byte) []Algorithm {
	var out []Algorithm
	// A 20-byte header, then descriptors of a 4-byte header and a length.
	for off := 20; off+4 <= len(b); {
		n := int(binary.BigEndian.Uint16(b[off+2:]))
		if n < 20 || off+4+n > len(b) {
			break
		}
		d := b[off : off+4+n]
		encryptC := d[4] & 0x03
		out = append(out, Algorithm{
			Index:   d[0],
			Code:    binary.BigEndian.Uint32(d[20:]),
			KeyBits: int(binary.BigEndian.Uint16(d[10:])) * 8,
			Usable:  encryptC == 0x02 && d[4]&0x80 != 0,
		})
		off += 4 + n
	}
	return out
}

// EncryptionStatus is the drive's current encryption setting.
type EncryptionStatus struct {
	EncryptMode    byte // 0 off, 1 external, 2 encrypt
	DecryptMode    byte // 0 off, 1 raw, 2 decrypt, 3 mixed
	AlgorithmIndex byte
	KeyInstance    uint32
	KeyID          []byte // key-associated data, LTFS stores its key ID here
}

// Encrypting reports whether data written now is encrypted.
func (s *EncryptionStatus) Encrypting() bool { return s.EncryptMode == encModeEncrypt }

// Decrypting reports whether encrypted data can be read.
func (s *EncryptionStatus) Decrypting() bool {
	return s.DecryptMode == decModeDecrypt || s.DecryptMode == decModeMixed
}

// KeyName formats the key ID the way LTFS key files write it (DKi=): three
// characters and nine bytes in hex. Other key IDs are shown as text if
// printable, else in hex.
func (s *EncryptionStatus) KeyName() string {
	return FormatKeyID(s.KeyID)
}

// FormatKeyID formats a key ID like LTFS key files do.
func FormatKeyID(id []byte) string {
	if len(id) == 0 {
		return ""
	}
	if len(id) == ltfsKeyIDLength && printable(id[:ltfsKeyIDASCIILength]) == string(id[:ltfsKeyIDASCIILength]) {
		return string(id[:ltfsKeyIDASCIILength]) + hex.EncodeToString(id[ltfsKeyIDASCIILength:])
	}
	if p := printable(id); len(p) == len(id) {
		return p
	}
	return hex.EncodeToString(id)
}

// ReadEncryptionStatus reads the drive's current encryption setting.
func ReadEncryptionStatus(d Device) (*EncryptionStatus, error) {
	b, err := readSecurityPage(d, pageEncStatus)
	if err != nil {
		return nil, err
	}
	return parseEncryptionStatus(b)
}

func parseEncryptionStatus(b []byte) (*EncryptionStatus, error) {
	if len(b) < 24 {
		return nil, errShort
	}
	s := &EncryptionStatus{
		EncryptMode:    b[5],
		DecryptMode:    b[6],
		AlgorithmIndex: b[7],
		KeyInstance:    binary.BigEndian.Uint32(b[8:]),
	}
	for off := 24; off+4 <= len(b); {
		typ := b[off]
		n := int(binary.BigEndian.Uint16(b[off+2:]))
		if off+4+n > len(b) {
			break
		}
		if (typ == kadTypeAuth || typ == kadTypeUnauth) && s.KeyID == nil {
			s.KeyID = append([]byte(nil), b[off+4:off+4+n]...)
		}
		off += 4 + n
	}
	return s, nil
}
