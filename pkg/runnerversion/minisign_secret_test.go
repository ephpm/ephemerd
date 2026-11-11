package runnerversion

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestSecretKeyRoundTrip(t *testing.T) {
	pk, sk, err := GenerateKeypair(nil)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Public().KeyID != pk.KeyID || !sk.Public().Key.Equal(pk.Key) {
		t.Fatal("Public() does not match the generated public key")
	}

	// .key file -> parse -> same key.
	text := sk.Encode("test key")
	if !strings.HasPrefix(text, "untrusted comment: test key\n") {
		t.Fatalf("encoded file header: %q", text)
	}
	got, err := ParseSecretKey(text)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyID != sk.KeyID || !got.Key.Equal(sk.Key) {
		t.Fatal("secret key round trip mismatch")
	}
	// The bare base64 line is accepted too (how a CI secret is pasted).
	if _, err := ParseSecretKey(strings.Split(text, "\n")[1]); err != nil {
		t.Fatalf("bare line: %v", err)
	}

	// .pub file -> parse -> verifies what the secret key signs.
	pub, err := ParsePublicKey(pk.EncodeFile(""))
	if err != nil {
		t.Fatal(err)
	}
	data := manifestJSON(t, validManifest("2.338.2", "2.338.0", zeroSHA))
	sig := sk.Sign(data, "timestamp:1\tfile:runner-manifest.json\thashed")
	if !strings.Contains(sig, "trusted comment: timestamp:1") {
		t.Fatalf("signature file: %q", sig)
	}
	m, err := pub.VerifyAndParse(data, sig)
	if err != nil || m.Version != "2.338.2" {
		t.Fatalf("VerifyAndParse: %v %+v", err, m)
	}
	// Any other key fails closed.
	other, _, _ := GenerateKeypair(nil)
	if err := other.Verify(data, sig); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("other key: want ErrKeyMismatch, got %v", err)
	}
}

func TestParseSecretKeyRejects(t *testing.T) {
	_, sk, err := GenerateKeypair(nil)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Split(sk.Encode(""), "\n")[1]
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(f func([]byte)) string {
		b := append([]byte(nil), raw...)
		f(b)
		return base64.StdEncoding.EncodeToString(b)
	}
	cases := map[string]struct {
		text string
		want string
	}{
		"empty":     {"untrusted comment: x\n", "empty"},
		"garbage":   {"!!!!", "illegal base64"},
		"short":     {base64.StdEncoding.EncodeToString(raw[:50]), "not a minisign"},
		"encrypted": {mutate(func(b []byte) { copy(b[2:4], "Sc") }), "password-protected"},
		"bad kdf":   {mutate(func(b []byte) { copy(b[2:4], "ZZ") }), "kdf"},
		"bad chk":   {mutate(func(b []byte) { copy(b[4:6], "ZZ") }), "checksum algorithm"},
		"corrupt":   {mutate(func(b []byte) { b[len(b)-40] ^= 0xff }), "checksum mismatch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSecretKey(tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
	if _, err := ParseSecretKey(mutate(func(b []byte) { copy(b[2:4], "Sc") })); !errors.Is(err, ErrEncryptedSecretKey) {
		t.Fatalf("encrypted key must be ErrEncryptedSecretKey, got %v", err)
	}
}

// The on-disk layout must stay byte-compatible with minisign: 158 bytes,
// "Ed", kdf none, "B2".
func TestSecretKeyLayout(t *testing.T) {
	_, sk, err := GenerateKeypair(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Split(sk.Encode(""), "\n")[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 158 || string(raw[:2]) != "Ed" || string(raw[2:4]) != "\x00\x00" || string(raw[4:6]) != "B2" {
		t.Fatalf("layout: len=%d head=%q", len(raw), raw[:6])
	}
}
