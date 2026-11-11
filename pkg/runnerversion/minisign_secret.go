package runnerversion

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// Minisign SECRET keys, so the manifest pipeline can sign in pure Go (CI
// has no minisign binary) and so a keypair can be generated with the same
// code that verifies it. Only the UNENCRYPTED on-disk form is supported —
// what `minisign -G -W` writes. A password-protected key (scrypt-wrapped)
// is rejected with a clear error: the CI secret is a machine credential
// with nobody to type a password, and supporting both forms doubles the
// parser for no gain.
//
// Secret key file:
//
//	untrusted comment: <anything>
//	<base64: "Ed" | kdf_alg[2] | chk_alg[2] | kdf_salt[32] | opslimit[8] |
//	          memlimit[8] | key_id[8] | ed25519 secret key[64] | checksum[32]>
//
// kdf_alg is "Sc" (scrypt, encrypted) or "\0\0" (none). chk_alg is "B2":
// checksum = BLAKE2b-256("Ed" | key_id | secret key). The layout is the
// one minisign has used since 0.7; files produced here load in minisign
// and files from `minisign -G -W` load here.

// SecretKey is a parsed minisign secret key.
type SecretKey struct {
	KeyID [8]byte
	Key   ed25519.PrivateKey
}

const (
	minisignSecLen = 2 + 2 + 2 + 32 + 8 + 8 + 8 + ed25519.PrivateKeySize + 32
	secKDFNone     = "\x00\x00"
	secKDFScrypt   = "Sc"
	secChkBlake2b  = "B2"
)

// ErrEncryptedSecretKey is returned for a password-protected key.
var ErrEncryptedSecretKey = errors.New("minisign secret key is password-protected; only unencrypted keys (minisign -G -W, or mage manifest:keygen) are supported")

// GenerateKeypair makes a fresh minisign keypair with a random key id.
// rnd may be nil for crypto/rand.
func GenerateKeypair(rnd io.Reader) (*PublicKey, *SecretKey, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	pub, priv, err := ed25519.GenerateKey(rnd)
	if err != nil {
		return nil, nil, fmt.Errorf("generating ed25519 key: %w", err)
	}
	var id [8]byte
	if _, err := io.ReadFull(rnd, id[:]); err != nil {
		return nil, nil, fmt.Errorf("generating key id: %w", err)
	}
	return &PublicKey{KeyID: id, Key: pub}, &SecretKey{KeyID: id, Key: priv}, nil
}

// Public returns the matching public key.
func (sk *SecretKey) Public() *PublicKey {
	return &PublicKey{KeyID: sk.KeyID, Key: sk.Key.Public().(ed25519.PublicKey)}
}

func secretChecksum(keyID [8]byte, key ed25519.PrivateKey) [32]byte {
	buf := make([]byte, 0, 2+8+ed25519.PrivateKeySize)
	buf = append(buf, "Ed"...)
	buf = append(buf, keyID[:]...)
	buf = append(buf, key...)
	return blake2b.Sum256(buf)
}

// Encode returns the secret key as an unencrypted minisign .key file.
func (sk *SecretKey) Encode(untrustedComment string) string {
	raw := make([]byte, 0, minisignSecLen)
	raw = append(raw, "Ed"...)
	raw = append(raw, secKDFNone...)
	raw = append(raw, secChkBlake2b...)
	raw = append(raw, make([]byte, 32+8+8)...) // salt, opslimit, memlimit: unused without a KDF
	raw = append(raw, sk.KeyID[:]...)
	raw = append(raw, sk.Key...)
	sum := secretChecksum(sk.KeyID, sk.Key)
	raw = append(raw, sum[:]...)
	if untrustedComment == "" {
		untrustedComment = "minisign unencrypted secret key"
	}
	return "untrusted comment: " + untrustedComment + "\n" + base64.StdEncoding.EncodeToString(raw) + "\n"
}

// ParseSecretKey accepts a whole .key file or just its base64 line.
func ParseSecretKey(text string) (*SecretKey, error) {
	line := firstPayloadLine(text)
	if line == "" {
		return nil, errors.New("secret key: empty")
	}
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	if len(raw) != minisignSecLen || string(raw[:2]) != "Ed" {
		return nil, errors.New("secret key: not a minisign Ed25519 secret key")
	}
	switch string(raw[2:4]) {
	case secKDFNone:
	case secKDFScrypt:
		return nil, ErrEncryptedSecretKey
	default:
		return nil, fmt.Errorf("secret key: unknown kdf algorithm %q", raw[2:4])
	}
	if string(raw[4:6]) != secChkBlake2b {
		return nil, fmt.Errorf("secret key: unknown checksum algorithm %q", raw[4:6])
	}
	off := 6 + 32 + 8 + 8
	sk := &SecretKey{Key: ed25519.PrivateKey(append([]byte(nil), raw[off+8:off+8+ed25519.PrivateKeySize]...))}
	copy(sk.KeyID[:], raw[off:off+8])
	want := secretChecksum(sk.KeyID, sk.Key)
	if string(want[:]) != string(raw[off+8+ed25519.PrivateKeySize:]) {
		return nil, errors.New("secret key: checksum mismatch (corrupt or truncated)")
	}
	return sk, nil
}

// EncodeFile returns the public key as a minisign .pub file.
func (pk *PublicKey) EncodeFile(untrustedComment string) string {
	if untrustedComment == "" {
		untrustedComment = "minisign public key " + strings.ToUpper(fmt.Sprintf("%x", pk.KeyID))
	}
	return "untrusted comment: " + untrustedComment + "\n" + pk.String() + "\n"
}

// Sign signs data the way current minisign does (pre-hashed, "ED") with a
// trusted comment, returning the .minisig file text.
func (sk *SecretKey) Sign(data []byte, trustedComment string) string {
	return Sign(sk.Key, sk.KeyID, data, trustedComment, true)
}
