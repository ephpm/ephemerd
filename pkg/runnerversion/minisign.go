package runnerversion

import (
	"bytes"
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// Minisign (https://jedisct1.github.io/minisign/) verification in pure Go.
//
// Public key file:
//
//	untrusted comment: <anything>
//	<base64: "Ed" | key_id[8] | ed25519 public key[32]>
//
// Signature file:
//
//	untrusted comment: <anything>
//	<base64: alg[2] | key_id[8] | signature[64]>
//	trusted comment: <text>
//	<base64: global signature[64] over signature || trusted comment text>
//
// alg is "Ed" (signature over the raw file) or "ED" (signature over
// BLAKE2b-512 of the file; what current minisign emits). Both the file
// signature and the global signature must verify under the same key, and
// the key id must match the embedded key — a signature from any other
// minisign key is rejected before any Ed25519 arithmetic.

//go:embed keys/runner-manifest.pub
var embeddedPublicKeyText string

var (
	// ErrNoPublicKey is returned when the embedded key file holds no key
	// (the placeholder shipped until an operator generates one). Every
	// verify fails closed in that state.
	ErrNoPublicKey = errors.New("no runner-manifest public key embedded")
	// ErrBadSignature is returned when a signature does not verify.
	ErrBadSignature = errors.New("runner manifest signature invalid")
	// ErrKeyMismatch is returned when the signature names a different key.
	ErrKeyMismatch = errors.New("runner manifest signed by a different key")
)

const (
	minisignPubLen = 2 + 8 + ed25519.PublicKeySize
	minisignSigLen = 2 + 8 + ed25519.SignatureSize
)

// PublicKey is a parsed minisign public key.
type PublicKey struct {
	KeyID [8]byte
	Key   ed25519.PublicKey
}

// EmbeddedPublicKey returns the key compiled into this binary.
func EmbeddedPublicKey() (*PublicKey, error) {
	return ParsePublicKey(embeddedPublicKeyText)
}

// ParsePublicKey accepts either a whole .pub file or just its base64 line.
func ParsePublicKey(text string) (*PublicKey, error) {
	line := firstPayloadLine(text)
	if line == "" {
		return nil, ErrNoPublicKey
	}
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	if len(raw) != minisignPubLen || string(raw[:2]) != "Ed" {
		return nil, fmt.Errorf("public key: not a minisign Ed25519 key")
	}
	pk := &PublicKey{Key: ed25519.PublicKey(raw[10:])}
	copy(pk.KeyID[:], raw[2:10])
	return pk, nil
}

// String renders the key in minisign's base64 form.
func (pk *PublicKey) String() string {
	raw := make([]byte, 0, minisignPubLen)
	raw = append(raw, 'E', 'd')
	raw = append(raw, pk.KeyID[:]...)
	raw = append(raw, pk.Key...)
	return base64.StdEncoding.EncodeToString(raw)
}

// Signature is a parsed minisign .minisig file.
type Signature struct {
	Prehashed      bool
	KeyID          [8]byte
	Sig            []byte
	TrustedComment string
	GlobalSig      []byte
}

// ParseSignature parses a .minisig file.
func ParseSignature(text string) (*Signature, error) {
	var payload []string
	var trusted string
	seenTrusted := false
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case strings.HasPrefix(l, "untrusted comment:"):
		case strings.HasPrefix(l, "trusted comment:"):
			trusted = strings.TrimSpace(strings.TrimPrefix(l, "trusted comment:"))
			seenTrusted = true
		case strings.TrimSpace(l) == "":
		default:
			payload = append(payload, strings.TrimSpace(l))
		}
	}
	if len(payload) != 2 || !seenTrusted {
		return nil, fmt.Errorf("%w: expected signature, trusted comment and global signature lines", ErrBadSignature)
	}
	raw, err := base64.StdEncoding.DecodeString(payload[0])
	if err != nil {
		return nil, fmt.Errorf("%w: signature: %w", ErrBadSignature, err)
	}
	if len(raw) != minisignSigLen {
		return nil, fmt.Errorf("%w: signature is %d bytes, want %d", ErrBadSignature, len(raw), minisignSigLen)
	}
	s := &Signature{TrustedComment: trusted}
	switch string(raw[:2]) {
	case "ED":
		s.Prehashed = true
	case "Ed":
	default:
		return nil, fmt.Errorf("%w: unknown algorithm %q", ErrBadSignature, raw[:2])
	}
	copy(s.KeyID[:], raw[2:10])
	s.Sig = raw[10:]
	g, err := base64.StdEncoding.DecodeString(payload[1])
	if err != nil {
		return nil, fmt.Errorf("%w: global signature: %w", ErrBadSignature, err)
	}
	if len(g) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: global signature is %d bytes", ErrBadSignature, len(g))
	}
	s.GlobalSig = g
	return s, nil
}

// Verify checks sigText (a .minisig file) over data under this key.
func (pk *PublicKey) Verify(data []byte, sigText string) error {
	sig, err := ParseSignature(sigText)
	if err != nil {
		return err
	}
	if sig.KeyID != pk.KeyID {
		return ErrKeyMismatch
	}
	msg := data
	if sig.Prehashed {
		h := blake2b.Sum512(data)
		msg = h[:]
	}
	if !ed25519.Verify(pk.Key, msg, sig.Sig) {
		return fmt.Errorf("%w: file signature", ErrBadSignature)
	}
	global := make([]byte, 0, len(sig.Sig)+len(sig.TrustedComment))
	global = append(global, sig.Sig...)
	global = append(global, sig.TrustedComment...)
	if !ed25519.Verify(pk.Key, global, sig.GlobalSig) {
		return fmt.Errorf("%w: global signature", ErrBadSignature)
	}
	return nil
}

// VerifyAndParse verifies the signature FIRST and only then parses the
// manifest. Unverified bytes never reach the JSON decoder.
func (pk *PublicKey) VerifyAndParse(data []byte, sigText string) (*Manifest, error) {
	if err := pk.Verify(data, sigText); err != nil {
		return nil, err
	}
	return ParseManifest(data)
}

// Sign produces a .minisig file for data with a private key. It exists so
// tests (and an operator without the minisign binary) can produce
// signatures this verifier accepts; the fleet's real manifests are signed
// offline with minisign itself.
func Sign(priv ed25519.PrivateKey, keyID [8]byte, data []byte, trustedComment string, prehash bool) string {
	msg := data
	alg := "Ed"
	if prehash {
		h := blake2b.Sum512(data)
		msg = h[:]
		alg = "ED"
	}
	sig := ed25519.Sign(priv, msg)
	raw := make([]byte, 0, minisignSigLen)
	raw = append(raw, alg...)
	raw = append(raw, keyID[:]...)
	raw = append(raw, sig...)
	global := ed25519.Sign(priv, append(bytes.Clone(sig), trustedComment...))
	var b strings.Builder
	b.WriteString("untrusted comment: signature from ephemerd runnerversion.Sign\n")
	b.WriteString(base64.StdEncoding.EncodeToString(raw))
	b.WriteString("\ntrusted comment: ")
	b.WriteString(trustedComment)
	b.WriteString("\n")
	b.WriteString(base64.StdEncoding.EncodeToString(global))
	b.WriteString("\n")
	return b.String()
}

// firstPayloadLine returns the first non-comment, non-empty line.
func firstPayloadLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "untrusted comment:") || strings.HasPrefix(l, "#") {
			continue
		}
		return l
	}
	return ""
}
