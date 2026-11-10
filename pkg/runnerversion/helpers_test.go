package runnerversion

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeArchive is a tiny tar.gz with run.sh in it, plus its sha256.
func fakeArchive(t *testing.T, content string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(content)
	if err := tw.WriteHeader(&tar.Header{Name: "run.sh", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// testKey makes an ed25519 keypair in minisign form.
func testKey(t *testing.T) (*PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pk := &PublicKey{Key: pub}
	copy(pk.KeyID[:], []byte("keyid123"))
	return pk, priv
}

func validManifest(version, floor, sha string) *Manifest {
	return &Manifest{
		SchemaVersion: 1,
		Version:       version,
		Floor:         floor,
		Channel:       ChannelStable,
		PublishedAt:   time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
		Platforms: map[string]PlatformArtifact{
			"linux-x64": {URL: "https://example.com/dl/" + ArchiveFilename("linux-x64", version), SHA256: sha},
			"win-x64":   {URL: "https://example.com/dl/" + ArchiveFilename("win-x64", version), SHA256: sha},
			"osx-arm64": {URL: "https://example.com/dl/" + ArchiveFilename("osx-arm64", version), SHA256: sha},
		},
	}
}

func manifestJSON(t *testing.T, m *Manifest) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const zeroSHA = "0000000000000000000000000000000000000000000000000000000000000000"

// serveBytes is an http.Handler that writes data; a failed write fails the test.
func serveBytes(t *testing.T, data []byte, hits *atomic.Int32) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if _, err := w.Write(data); err != nil {
			t.Errorf("serving test bytes: %v", err)
		}
	})
}
