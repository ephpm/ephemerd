package dind

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// --- normalizeServer tests ---

func TestNormalizeServer(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "docker.io"},
		{"docker.io", "docker.io"},
		{"index.docker.io", "docker.io"},
		{"registry-1.docker.io", "docker.io"},
		{"https://index.docker.io/v1/", "docker.io"},
		{"https://index.docker.io/v2/", "docker.io"},
		{"http://localhost:5000", "localhost:5000"},
		{"ghcr.io", "ghcr.io"},
		{"https://ghcr.io", "ghcr.io"},
		{"  ghcr.io  ", "ghcr.io"},
		{"data.forgejo.org", "data.forgejo.org"},
		{"my-registry.example.com:5000/path", "my-registry.example.com:5000"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := normalizeServer(tt.in)
			if got != tt.want {
				t.Errorf("normalizeServer(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// --- registryHostFromRef tests ---

func TestRegistryHostFromRef(t *testing.T) {
	tests := []struct {
		ref, want string
	}{
		// Bare names default to docker.io.
		{"alpine", "docker.io"},
		{"alpine:latest", "docker.io"},
		{"library/alpine:latest", "docker.io"},
		{"myorg/myimage", "docker.io"},
		{"myorg/myimage:tag", "docker.io"},
		// Explicit hosts (must contain a "." or ":" or be "localhost").
		{"ghcr.io/owner/repo:latest", "ghcr.io"},
		{"docker.io/library/alpine:latest", "docker.io"},
		{"localhost/test:latest", "localhost"},
		// registryHostFromRef strips tag at the first ":" before parsing,
		// so port-bearing host:port refs collapse to just the host.
		{"localhost:5000/test:latest", "localhost"},
		{"my-registry.example.com:5000/owner/repo:tag", "my-registry.example.com"},
		// Digest separators get stripped along with the tag.
		{"ghcr.io/owner/repo@sha256:abc", "ghcr.io"},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got := registryHostFromRef(tt.ref)
			if got != tt.want {
				t.Errorf("registryHostFromRef(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

// --- qualifyDockerHubRef tests ---

func TestQualifyDockerHubRef(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		// Single-segment unqualified names get library/ prepended.
		{"alpine", "docker.io/library/alpine"},
		// Single-segment with tag — qualifyDockerHubRef sees the ":" and
		// thinks "alpine:latest" is already a host, so it passes through
		// unchanged. This is a known limitation; passing "alpine" without
		// a tag is the supported form when callers want library/ added.
		{"alpine:latest", "alpine:latest"},
		// Two-segment names get docker.io/ prepended.
		{"myorg/myimage", "docker.io/myorg/myimage"},
		{"myorg/myimage:tag", "docker.io/myorg/myimage:tag"},
		// Already-qualified refs pass through.
		{"ghcr.io/owner/repo:latest", "ghcr.io/owner/repo:latest"},
		{"docker.io/library/alpine:latest", "docker.io/library/alpine:latest"},
		{"localhost/test:latest", "localhost/test:latest"},
		{"localhost:5000/test:latest", "localhost:5000/test:latest"},
		{"my-registry.example.com:5000/owner/repo:tag", "my-registry.example.com:5000/owner/repo:tag"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := qualifyDockerHubRef(tt.in)
			if got != tt.want {
				t.Errorf("qualifyDockerHubRef(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// --- decodeAuthHeader tests ---

func TestDecodeAuthHeader_StdEncoding(t *testing.T) {
	cfg := authConfig{Username: "alice", Password: "secret"}
	raw, _ := json.Marshal(cfg)
	hdr := base64.StdEncoding.EncodeToString(raw)

	got, ok := decodeAuthHeader(hdr)
	if !ok {
		t.Fatal("expected decode ok")
	}
	if got.Username != "alice" || got.Password != "secret" {
		t.Errorf("got = %+v, want alice/secret", got)
	}
}

func TestDecodeAuthHeader_URLEncoding(t *testing.T) {
	cfg := authConfig{Username: "bob", Password: "p?ssw/rd+"}
	raw, _ := json.Marshal(cfg)
	hdr := base64.URLEncoding.EncodeToString(raw)

	got, ok := decodeAuthHeader(hdr)
	if !ok {
		t.Fatal("expected decode ok")
	}
	if got.Username != "bob" || got.Password != "p?ssw/rd+" {
		t.Errorf("got = %+v, want bob/...", got)
	}
}

func TestDecodeAuthHeader_RawEncodings(t *testing.T) {
	cfg := authConfig{Username: "charlie", Password: "x"}
	raw, _ := json.Marshal(cfg)

	for name, enc := range map[string]func([]byte) string{
		"raw-std": base64.RawStdEncoding.EncodeToString,
		"raw-url": base64.RawURLEncoding.EncodeToString,
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := decodeAuthHeader(enc(raw))
			if !ok {
				t.Fatal("expected decode ok")
			}
			if got.Username != "charlie" {
				t.Errorf("Username = %q, want charlie", got.Username)
			}
		})
	}
}

func TestDecodeAuthHeader_Garbage(t *testing.T) {
	// Not base64 at all, decoder loop should fail through every variant.
	if _, ok := decodeAuthHeader("not base64@@@"); ok {
		t.Error("expected garbage input to fail decode")
	}
}

func TestDecodeAuthHeader_NotJSON(t *testing.T) {
	hdr := base64.StdEncoding.EncodeToString([]byte("hello world"))
	if _, ok := decodeAuthHeader(hdr); ok {
		t.Error("expected non-JSON payload to fail decode")
	}
}

func TestDecodeAuthHeader_StripsBOMAndWhitespace(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	cfg := authConfig{
		Username: bom + "ephpm" + "\r\n",
		Password: bom + "secret" + " ",
	}
	raw, _ := json.Marshal(cfg)
	hdr := base64.StdEncoding.EncodeToString(raw)

	got, ok := decodeAuthHeader(hdr)
	if !ok {
		t.Fatal("expected decode ok")
	}
	if got.Username != "ephpm" {
		t.Errorf("Username = %q, want ephpm (BOM/whitespace stripped)", got.Username)
	}
	if got.Password != "secret" {
		t.Errorf("Password = %q, want secret (BOM/whitespace stripped)", got.Password)
	}
}

// --- sanitizeCred tests ---

func TestSanitizeCred(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"plain", "plain"},
		{bom + "plain", "plain"},
		{bom + "plain" + "\r\n", "plain"},
		{"  plain  ", "plain"},
		{"\tplain\n", "plain"},
		{bom + "\tplain   \r\n", "plain"},
	}

	for _, tt := range tests {
		got := sanitizeCred(tt.in)
		if got != tt.want {
			t.Errorf("sanitizeCred(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- safePrefix tests ---

func TestSafePrefix(t *testing.T) {
	tests := []struct {
		s    string
		n    int
		want string
	}{
		{"hello world", 5, "hello"},
		{"hi", 5, "hi"},
		{"", 5, ""},
		{"abc", 0, ""},
		{"abc", 3, "abc"},
		{"abcdef", 100, "abcdef"},
	}
	for _, tt := range tests {
		got := safePrefix(tt.s, tt.n)
		if got != tt.want {
			t.Errorf("safePrefix(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
		}
	}
}

// --- tailHex tests ---

func TestTailHex(t *testing.T) {
	tests := []struct {
		s, want string
	}{
		{"", ""},
		{"a", "61"},
		{"ab", "61 62"},
		{"hello", "68 65 6c 6c 6f"},
		// Long input — only the last 8 bytes hexed, space-separated.
		{"0123456789abcdef", "38 39 61 62 63 64 65 66"},
	}
	for _, tt := range tests {
		got := tailHex(tt.s)
		if got != tt.want {
			t.Errorf("tailHex(%q) = %q, want %q", tt.s, got, tt.want)
		}
	}
}

// --- authCache tests ---

func TestAuthCache_PutGetNormalizes(t *testing.T) {
	c := authCache{}
	cfg := authConfig{Username: "alice", Password: "x"}

	c.put("https://index.docker.io/v1/", cfg)

	for _, key := range []string{
		"docker.io",
		"index.docker.io",
		"registry-1.docker.io",
		"https://index.docker.io/v1/",
	} {
		got, ok := c.get(key)
		if !ok {
			t.Errorf("cache miss for normalized key %q", key)
			continue
		}
		if got.Username != "alice" {
			t.Errorf("Username for %q = %q, want alice", key, got.Username)
		}
	}
}

func TestAuthCache_DistinctHosts(t *testing.T) {
	c := authCache{}
	c.put("ghcr.io", authConfig{Username: "g"})
	c.put("docker.io", authConfig{Username: "d"})

	g, ok := c.get("ghcr.io")
	if !ok || g.Username != "g" {
		t.Errorf("ghcr.io = %+v, ok=%v", g, ok)
	}
	d, ok := c.get("docker.io")
	if !ok || d.Username != "d" {
		t.Errorf("docker.io = %+v, ok=%v", d, ok)
	}
}

func TestAuthCache_OverwritesOnSecondPut(t *testing.T) {
	c := authCache{}
	c.put("ghcr.io", authConfig{Username: "first"})
	c.put("ghcr.io", authConfig{Username: "second"})

	got, ok := c.get("ghcr.io")
	if !ok {
		t.Fatal("get failed")
	}
	if got.Username != "second" {
		t.Errorf("Username = %q, want second", got.Username)
	}
}

// --- resolveAuthForRef tests ---

func TestResolveAuthForRef_HeaderTakesPrecedence(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(s.Stop)

	s.auth.put("docker.io", authConfig{Username: "cached", Password: "cached-pass"})

	hdrCfg := authConfig{Username: "header", Password: "header-pass"}
	hdrJSON, _ := json.Marshal(hdrCfg)
	req, _ := http.NewRequest(http.MethodPost, "http://docker/x", nil)
	req.Header.Set("X-Registry-Auth", base64.StdEncoding.EncodeToString(hdrJSON))

	got, ok := s.resolveAuthForRef(req, "myorg/myimage:tag")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Username != "header" {
		t.Errorf("Username = %q, want header (header should take precedence)", got.Username)
	}
}

func TestResolveAuthForRef_FallsBackToCache(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(s.Stop)

	s.auth.put("ghcr.io", authConfig{Username: "from-cache", Password: "x"})

	req, _ := http.NewRequest(http.MethodPost, "http://docker/x", nil)
	got, ok := s.resolveAuthForRef(req, "ghcr.io/owner/repo:tag")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.Username != "from-cache" {
		t.Errorf("Username = %q, want from-cache", got.Username)
	}
}

func TestResolveAuthForRef_NoAuth(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(s.Stop)

	req, _ := http.NewRequest(http.MethodPost, "http://docker/x", nil)
	_, ok := s.resolveAuthForRef(req, "ghcr.io/owner/repo:tag")
	if ok {
		t.Error("expected ok=false when no header and no cache entry")
	}
}

func TestResolveAuthForRef_InvalidHeaderFallsBackToCache(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(s.Stop)

	s.auth.put("docker.io", authConfig{Username: "cached", Password: "x"})

	req, _ := http.NewRequest(http.MethodPost, "http://docker/x", nil)
	// Invalid base64 — decode fails, so fall through to the cache.
	req.Header.Set("X-Registry-Auth", "!!! not base64 !!!")

	got, ok := s.resolveAuthForRef(req, "myorg/myimage:tag")
	if !ok {
		t.Fatal("expected fallback to cache when header invalid")
	}
	if got.Username != "cached" {
		t.Errorf("Username = %q, want cached", got.Username)
	}
}

// --- handleAuth integration sanity ---

func TestHandleAuth_PowerShellCRLF(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(s.Stop)
	cli := dialServer(s)

	body := `{"username":"ephpm","password":"secret\r\n","serveraddress":"https://index.docker.io/v1/"}`
	req, _ := http.NewRequest(http.MethodPost, "http://docker/auth", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("/auth: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got, ok := s.auth.get("docker.io")
	if !ok {
		t.Fatal("nothing cached")
	}
	if got.Password != "secret" {
		t.Errorf("password = %q, want secret (CRLF stripped)", got.Password)
	}
}
