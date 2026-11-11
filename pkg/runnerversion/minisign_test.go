package runnerversion

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func zeroTime() time.Time { return time.Time{} }

func TestMinisignRoundTrip(t *testing.T) {
	pk, priv := testKey(t)
	data := manifestJSON(t, validManifest("2.338.2", "2.338.0", zeroSHA))

	for _, prehash := range []bool{true, false} {
		sig := Sign(priv, pk.KeyID, data, "timestamp:1 file:runner-manifest.json", prehash)
		if err := pk.Verify(data, sig); err != nil {
			t.Fatalf("prehash=%v verify: %v", prehash, err)
		}
		m, err := pk.VerifyAndParse(data, sig)
		if err != nil || m.Version != "2.338.2" {
			t.Fatalf("prehash=%v VerifyAndParse: %v %+v", prehash, err, m)
		}
		// Tampered content.
		tampered := []byte(strings.Replace(string(data), "2.338.2", "2.338.3", 1))
		if err := pk.Verify(tampered, sig); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("prehash=%v tampered data should fail: %v", prehash, err)
		}
		// Tampered trusted comment breaks the global signature only.
		bad := strings.Replace(sig, "timestamp:1", "timestamp:2", 1)
		if err := pk.Verify(data, bad); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("prehash=%v tampered trusted comment should fail: %v", prehash, err)
		}
	}
}

func TestMinisignKeyMismatch(t *testing.T) {
	pk, priv := testKey(t)
	other, _ := testKey(t)
	other.KeyID = [8]byte{9, 9, 9, 9, 9, 9, 9, 9}
	data := []byte("hello")
	sig := Sign(priv, pk.KeyID, data, "c", true)
	if err := other.Verify(data, sig); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("expected ErrKeyMismatch, got %v", err)
	}
	// Same key id, different key: signature fails, not key mismatch.
	other.KeyID = pk.KeyID
	if err := other.Verify(data, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}
}

func TestPublicKeyParseAndString(t *testing.T) {
	pk, _ := testKey(t)
	text := "untrusted comment: minisign public key\n" + pk.String() + "\n"
	got, err := ParsePublicKey(text)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyID != pk.KeyID || !got.Key.Equal(pk.Key) {
		t.Fatal("round trip mismatch")
	}
	if _, err := ParsePublicKey("untrusted comment: nothing here\n"); !errors.Is(err, ErrNoPublicKey) {
		t.Fatalf("expected ErrNoPublicKey, got %v", err)
	}
	if _, err := ParsePublicKey("AAAA"); err == nil {
		t.Fatal("expected error for short key")
	}
}

// The trust root compiled into every ephemerd build. Its key id is pinned
// here so a rotation is a deliberate, reviewed change to BOTH files: the
// matching secret key is the RUNNER_MANIFEST_MINISIGN_KEY repository
// secret that signs the published manifest.
const embeddedKeyID = "CCFDB74C12C7F433"

func TestEmbeddedPublicKeyIsReal(t *testing.T) {
	pk, err := EmbeddedPublicKey()
	if err != nil {
		t.Fatalf("embedded key must parse: %v", err)
	}
	if got := strings.ToUpper(hex.EncodeToString(pk.KeyID[:])); got != embeddedKeyID {
		t.Fatalf("embedded key id %s, want %s (rotation must update this test deliberately)", got, embeddedKeyID)
	}
	// A manifest signed by any OTHER key fails closed under it.
	_, sk, err := GenerateKeypair(nil)
	if err != nil {
		t.Fatal(err)
	}
	data := manifestJSON(t, validManifest("2.338.2", "2.338.0", zeroSHA))
	if err := pk.Verify(data, sk.Sign(data, "c")); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("foreign signature must be ErrKeyMismatch, got %v", err)
	}
}

func TestParseSignatureRejectsMalformed(t *testing.T) {
	for name, text := range map[string]string{
		"empty":              "",
		"no trusted comment": "untrusted comment: x\nAAAA\nBBBB\n",
		"one payload":        "untrusted comment: x\nAAAA\ntrusted comment: y\n",
		"bad base64":         "untrusted comment: x\n!!!!\ntrusted comment: y\nAAAA\n",
	} {
		if _, err := ParseSignature(text); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: expected ErrBadSignature, got %v", name, err)
		}
	}
}
