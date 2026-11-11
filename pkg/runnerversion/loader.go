package runnerversion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ManifestLoader produces a VERIFIED manifest. Implementations must not
// return a manifest whose signature did not check out.
type ManifestLoader interface {
	Load(ctx context.Context) (*Manifest, error)
}

// maxManifestBytes bounds the manifest and signature downloads; a real
// manifest is under 2 KiB.
const maxManifestBytes = 64 << 10

// HTTPManifestLoader fetches runner-manifest.json and its detached
// .minisig over HTTPS and verifies the pair with Key.
type HTTPManifestLoader struct {
	URL    string
	Key    *PublicKey
	Client *http.Client
}

// ErrManifestFetch wraps transport failures so callers can count them
// separately from signature failures.
var ErrManifestFetch = errors.New("runner manifest fetch failed")

// Load fetches, verifies, parses.
func (l *HTTPManifestLoader) Load(ctx context.Context) (*Manifest, error) {
	if l.Key == nil {
		return nil, ErrNoPublicKey
	}
	if !strings.HasPrefix(l.URL, "https://") {
		return nil, fmt.Errorf("%w: manifest url must be https", ErrManifestFetch)
	}
	client := l.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	data, err := getBounded(ctx, client, l.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrManifestFetch, err)
	}
	sig, err := getBounded(ctx, client, l.URL+".minisig")
	if err != nil {
		return nil, fmt.Errorf("%w: signature: %w", ErrManifestFetch, err)
	}
	return l.Key.VerifyAndParse(data, string(sig))
}

func getBounded(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", url, maxManifestBytes)
	}
	return data, nil
}

// StaticManifestLoader returns a fixed, already-verified manifest. For
// tests and for the `ephemerd runner-manifest check` style tooling.
type StaticManifestLoader struct{ Manifest *Manifest }

// Load returns the manifest.
func (s StaticManifestLoader) Load(context.Context) (*Manifest, error) {
	if s.Manifest == nil {
		return nil, ErrManifestFetch
	}
	return s.Manifest, nil
}
