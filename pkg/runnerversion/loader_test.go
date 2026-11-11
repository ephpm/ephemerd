package runnerversion

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestHTTPManifestLoader(t *testing.T) {
	pk, priv := testKey(t)
	m := validManifest("2.338.2", "2.338.0", zeroSHA)
	data := manifestJSON(t, m)
	sig := Sign(priv, pk.KeyID, data, "runner-manifest", true)

	mux := http.NewServeMux()
	mux.Handle("/runner-manifest.json", serveBytes(t, data, nil))
	mux.Handle("/runner-manifest.json.minisig", serveBytes(t, []byte(sig), nil))
	mux.Handle("/bad.json", serveBytes(t, []byte(`{"schema_version":1}`), nil))
	mux.Handle("/bad.json.minisig", serveBytes(t, []byte(sig), nil))
	srv, client := httpsTestServer(t, mux)

	l := &HTTPManifestLoader{URL: srv.URL + "/runner-manifest.json", Key: pk, Client: client}
	got, err := l.Load(context.Background())
	if err != nil || got.Version != "2.338.2" {
		t.Fatalf("load: %v %+v", err, got)
	}

	// Signature for other bytes: rejected before parsing.
	l.URL = srv.URL + "/bad.json"
	if _, err := l.Load(context.Background()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}
	// Missing signature file.
	l.URL = srv.URL + "/nope.json"
	if _, err := l.Load(context.Background()); !errors.Is(err, ErrManifestFetch) {
		t.Fatalf("expected ErrManifestFetch, got %v", err)
	}
	// No key: fail closed without a request.
	l.Key = nil
	if _, err := l.Load(context.Background()); !errors.Is(err, ErrNoPublicKey) {
		t.Fatalf("expected ErrNoPublicKey, got %v", err)
	}
	// Plain http refused.
	l.Key = pk
	l.URL = "http://example.com/m.json"
	if _, err := l.Load(context.Background()); !errors.Is(err, ErrManifestFetch) {
		t.Fatalf("expected ErrManifestFetch for http, got %v", err)
	}
}
