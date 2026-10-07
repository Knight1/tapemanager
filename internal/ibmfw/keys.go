package ibmfw

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/pem"
	"fmt"
)

// The certificates IBM LTO drives check firmware signatures with: the drive
// application carries them and uses their public keys directly, without a
// chain and without looking at the validity dates. They hold public keys
// only and are the same in every drive.
//
//go:embed certs/*.pem
var certFiles embed.FS

// IBMKeys returns the keys for IBM LTO firmware signatures: tag 0x20
// (SHA-1) is checked with CN=L4H_FIRMWARE, tag 0x10 (SHA-256) with
// CN=TAPEFIRMWARE.
func IBMKeys() (Keys, error) {
	keys := Keys{}
	for tag, f := range map[byte]struct {
		file string
		hash crypto.Hash
	}{
		TagSigSHA1:   {"certs/L4H_FIRMWARE.pem", crypto.SHA1},
		TagSigSHA256: {"certs/TAPEFIRMWARE.pem", crypto.SHA256},
	} {
		c, err := loadCert(f.file)
		if err != nil {
			return nil, err
		}
		pub, ok := c.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%s: not an RSA key", f.file)
		}
		sum := sha256.Sum256(c.Raw)
		keys[tag] = Key{Name: c.Subject.CommonName, Hash: f.hash, Pub: pub, NotAfter: c.NotAfter, Fingerprint: hex.EncodeToString(sum[:])}
	}
	return keys, nil
}

func loadCert(name string) (*x509.Certificate, error) {
	b, err := certFiles.ReadFile(name)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no PEM certificate", name)
	}
	return x509.ParseCertificate(blk.Bytes)
}
