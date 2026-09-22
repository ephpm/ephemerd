//go:build livegithub

// Run with: go test -tags livegithub ./pkg/proxies/ghrel/ -run TestLive -v
//
// Talks to the REAL api.github.com. Kept behind a build tag so CI stays
// hermetic, but it is the only test that could have caught either of the two
// production breaks: both were about what GitHub actually returns, and every
// hermetic test in this package asserts against a fake that does not model the
// asset endpoint's content negotiation.
package ghrelproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// The exact artifact that broke the fleet twice: spc's zlib 1.3.2, whose
// sha256 spc pins. If the proxy serves anything other than the tarball --
// metadata JSON, an error document, a truncated body -- this fails the same
// way spc did.
const (
	zlibAssetPath = "/repos/madler/zlib/releases/assets/357391855"
	zlibSHA256    = "bb329a0a2cd0274d05519d61c667c062e06990d72e125ee2dfa8de64f0119d16"
)

func TestLiveGitHubAssetBytesMatchSPCHash(t *testing.T) {
	p := startProxy(t, Config{})
	base := "http://" + p.Addr()

	fetch := func(label string) string {
		resp, err := http.Get(base + zlibAssetPath)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", label, resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s: read: %v", label, err)
		}
		// The break-2 signature: a 200 whose body is the asset's metadata.
		var probe map[string]any
		if json.Unmarshal(body, &probe) == nil {
			if _, isMeta := probe["browser_download_url"]; isMeta || probe["url"] != nil {
				t.Fatalf("%s: served %d bytes of METADATA JSON, not the asset", label, len(body))
			}
		}
		sum := sha256.Sum256(body)
		t.Logf("%s: %d bytes, sha256=%s", label, len(body), hex.EncodeToString(sum[:]))
		return hex.EncodeToString(sum[:])
	}

	// First fetch populates the cache; second must serve identical bytes from
	// disk. A cache that stores the right thing and returns the wrong thing on
	// the hit path is just as broken.
	if got := fetch("cold"); got != zlibSHA256 {
		t.Fatalf("cold fetch sha256 = %s, want %s (this is exactly what spc rejects)", got, zlibSHA256)
	}
	if got := fetch("warm (cache hit)"); got != zlibSHA256 {
		t.Fatalf("warm fetch sha256 = %s, want %s", got, zlibSHA256)
	}
}
