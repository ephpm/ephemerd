package dind

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestPushResolverBearerFlow exercises the exact resolver+authorizer setup
// our handleImagePush uses, against a mock registry that does the Docker Hub
// Bearer challenge dance (HEAD → 401 with WWW-Authenticate → GET token →
// retry with Bearer). Asserts:
//
//   - The token endpoint sees a Basic auth header carrying our user+secret.
//   - The retry carries the bearer token.
//   - resolver.Resolve returns no error.
//
// Production was failing here with a 401 from auth.docker.io; this test
// pins the path so we can iterate on the resolver wiring without round-
// tripping through GitHub Actions.
func TestPushResolverBearerFlow(t *testing.T) {
	const (
		wantUser  = "ephpm"
		wantPass  = "secret-pat-1234567890"
		wantToken = "issued-bearer-abcdef"
	)

	var (
		sawTokenAuth   string
		sawRetryBearer string
	)

	mux := http.NewServeMux()

	// /v2/ — used by containerd to probe the registry. First call: 401 with
	// challenge. Second call: 200 with the bearer token.
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if hdr := r.Header.Get("Authorization"); strings.HasPrefix(hdr, "Bearer ") {
			sawRetryBearer = strings.TrimPrefix(hdr, "Bearer ")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="%s/auth/token",service="registry.docker.io",scope="repository:ephpm/ephemerd:pull"`,
				originBase(r)))
		w.WriteHeader(http.StatusUnauthorized)
	})

	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		sawTokenAuth = r.Header.Get("Authorization")
		// Validate Basic auth payload matches what we configured.
		if !strings.HasPrefix(sawTokenAuth, "Basic ") {
			http.Error(w, "expected Basic auth on token endpoint", http.StatusUnauthorized)
			return
		}
		raw := strings.TrimPrefix(sawTokenAuth, "Basic ")
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			http.Error(w, "bad base64", http.StatusBadRequest)
			return
		}
		userpass := string(decoded)
		if userpass != wantUser+":"+wantPass {
			http.Error(w, "credentials mismatch: "+userpass, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      wantToken,
			"expires_in": 60,
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Build the same authorizer + resolver our /push handler uses.
	creds := func(host string) (string, string, error) {
		return wantUser, wantPass, nil
	}
	resolver := docker.NewResolver(docker.ResolverOptions{
		Authorizer: docker.NewDockerAuthorizer(docker.WithAuthCreds(creds)),
		// Plain http for the test server; PlainHTTP true tells the
		// resolver not to upgrade to https.
		PlainHTTP: true,
	})

	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/ephpm/ephemerd:test"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Resolve drives the auth flow. We don't care about the descriptor — we
	// only care that the Bearer dance round-tripped.
	if _, _, err := resolver.Resolve(ctx, ref); err != nil {
		// "not found" is acceptable here — the mock returns 200 with no
		// manifest body. The error we want to NOT see is "401 Unauthorized"
		// from the token endpoint.
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "Unauthorized") {
			t.Fatalf("resolver.Resolve auth failed: %v", err)
		}
		t.Logf("resolver.Resolve returned non-auth error (acceptable): %v", err)
	}

	if sawTokenAuth == "" {
		t.Fatal("token endpoint never got Basic auth header — resolver didn't follow the challenge")
	}
	if sawRetryBearer != wantToken {
		t.Errorf("retry bearer = %q, want %q", sawRetryBearer, wantToken)
	}
}

// originBase returns "http://host:port" for the request's destination so the
// mock's WWW-Authenticate realm points back at itself.
func originBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// TestAuthCacheRoundtrip simulates the production /auth → /push cred flow:
//
//  1. Workflow runs `docker login` → POST /auth populates the cache.
//  2. Workflow runs `docker push` → /push reads X-Registry-Auth (preferred)
//     or falls back to the cache.
//
// Pins the cache key normalization (so "https://index.docker.io/v1/" cached
// from /auth is found by lookup keyed off "docker.io" derived from the
// image ref) and the X-Registry-Auth-takes-precedence rule. Both have
// caused our prod /push to look up empty creds in earlier iterations.
func TestAuthCacheRoundtrip(t *testing.T) {
	const (
		loginUser = "ephpm"
		loginPass = "secret-from-login"
	)

	s := newTestServer(t)
	cli := dialServer(s)

	// 1. POST /auth — what `docker login -u ephpm --password-stdin` sends.
	authReq, err := http.NewRequest(http.MethodPost, "http://docker/auth", strings.NewReader(
		fmt.Sprintf(`{"username":%q,"password":%q,"serveraddress":"https://index.docker.io/v1/"}`,
			loginUser, loginPass)))
	if err != nil {
		t.Fatalf("build auth req: %v", err)
	}
	authReq.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(authReq)
	if err != nil {
		t.Fatalf("POST /auth: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/auth status = %d, want 200", resp.StatusCode)
	}

	// 2a. Cache lookup keyed off the registry host that registryHostFromRef
	//     derives from "ephpm/ephemerd:tag" (i.e. "docker.io").
	cfg, ok := s.auth.get("docker.io")
	if !ok {
		t.Fatal("/auth body did not populate cache under normalized key 'docker.io'")
	}
	if cfg.Username != loginUser || cfg.Password != loginPass {
		t.Errorf("cached creds = (%q, len=%d), want (%q, len=%d)",
			cfg.Username, len(cfg.Password), loginUser, len(loginPass))
	}

	// 2b. resolveAuthForRef with no header → cache hit by ref-derived host.
	pushReq, _ := http.NewRequest(http.MethodPost, "http://docker/x", nil)
	got, ok := s.resolveAuthForRef(pushReq, "ephpm/ephemerd:runner-ci-windows")
	if !ok {
		t.Fatal("resolveAuthForRef cache miss for unqualified ref")
	}
	if got.Username != loginUser || got.Password != loginPass {
		t.Errorf("ref-derived lookup = %+v, want user=%q password_len=%d",
			redact(got), loginUser, len(loginPass))
	}

	// 2c. X-Registry-Auth header takes precedence — different creds win.
	hdrCfg := authConfig{Username: "alice", Password: "header-pass"}
	hdrJSON, _ := json.Marshal(hdrCfg)
	pushReq2, _ := http.NewRequest(http.MethodPost, "http://docker/x", nil)
	pushReq2.Header.Set("X-Registry-Auth", base64.StdEncoding.EncodeToString(hdrJSON))
	got2, ok := s.resolveAuthForRef(pushReq2, "ephpm/ephemerd:runner-ci-windows")
	if !ok {
		t.Fatal("resolveAuthForRef returned ok=false even with valid header")
	}
	if got2.Username != "alice" || got2.Password != "header-pass" {
		t.Errorf("header creds got dropped: got=%+v", redact(got2))
	}
}

// TestAuthCacheStripsWhitespace pins the workaround for PowerShell's pipe
// adding \r\n to the password when running:
//
//	$env:DOCKER_PASSWORD | docker login -u $u --password-stdin
//
// Without the trim, the cached cred carries trailing line-ending bytes,
// which Basic auth then sends to auth.docker.io and Docker Hub 401s. This
// is exactly the prod failure (logged password_len=39 vs the underlying
// 36-char dckr_pat_* token).
func TestAuthCacheStripsWhitespace(t *testing.T) {
	const cleanPass = "dckr_pat_aaaaaaaaaaaaaaaaaaaaaaaaaaa" // 36 chars
	const bom = "\xef\xbb\xbf"                               // UTF-8 BOM (3 bytes)
	for _, dirty := range []string{
		cleanPass + "\r\n",            // PowerShell pipe trailing CRLF
		cleanPass + "\n",              // bash echo
		" " + cleanPass + " ",         // accidental copy/paste padding
		"\t" + cleanPass,              // tab indent in YAML secret
		cleanPass + "   \r\n ",        // pathological trailing junk
		bom + cleanPass,               // PowerShell UTF-8 BOM (real prod failure)
		bom + cleanPass + "\r\n",      // BOM + CRLF combined
	} {
		t.Run(fmt.Sprintf("len=%d", len(dirty)), func(t *testing.T) {
			s := newTestServer(t)
			cli := dialServer(s)

			body, _ := json.Marshal(authConfig{
				Username:      "ephpm",
				Password:      dirty,
				Serveraddress: "https://index.docker.io/v1/",
			})
			req, _ := http.NewRequest(http.MethodPost, "http://docker/auth", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := cli.Do(req)
			if err != nil {
				t.Fatalf("POST /auth: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("/auth status = %d", resp.StatusCode)
			}

			cached, ok := s.auth.get("docker.io")
			if !ok {
				t.Fatal("nothing cached")
			}
			if cached.Password != cleanPass {
				t.Errorf("password not trimmed: cached len=%d, want %d (%q)",
					len(cached.Password), len(cleanPass), cached.Password)
			}
		})
	}
}

// redact strips secret material from authConfig for log output.
func redact(c authConfig) string {
	return fmt.Sprintf("user=%q password_len=%d identity_token_len=%d",
		c.Username, len(c.Password), len(c.IdentityToken))
}

// TestPushResolverRealDockerHub authenticates against the real auth.docker.io
// using DOCKER_USERNAME and DOCKER_PASSWORD from the env. Skipped when the
// vars are absent. Two sub-checks:
//
//   - "pull": Resolve an existing tag, exercising pull scope.
//   - "push": Acquire a Pusher (this triggers a token request with
//     "pull,push" scope), and attempt to start an upload. If the secret
//     lacks push rights or auth.docker.io rejects the push-scope token
//     request, we get the same 401 prod hits.
//
// When the prod build fails with 401 and these subtests pass, the bug is
// in our handler wiring (cache lookup, X-Registry-Auth decode). When push
// also 401s here, the credential isn't push-capable.
func TestPushResolverRealDockerHub(t *testing.T) {
	user := os.Getenv("DOCKER_USERNAME")
	pass := os.Getenv("DOCKER_PASSWORD")
	if user == "" || pass == "" {
		t.Skip("DOCKER_USERNAME / DOCKER_PASSWORD not set — skipping real-registry auth check")
	}
	repo := os.Getenv("DOCKER_TEST_REPO")
	if repo == "" {
		repo = user + "/ephemerd"
	}

	creds := func(host string) (string, string, error) {
		return user, pass, nil
	}
	resolver := docker.NewResolver(docker.ResolverOptions{
		Authorizer: docker.NewDockerAuthorizer(docker.WithAuthCreds(creds)),
	})

	t.Run("pull", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Resolve an existing tag. Pull scope. 401 here would mean our
		// Basic auth on auth.docker.io is being rejected.
		// runner-ci-linux-amd64 because that's a tag we know exists.
		ref := "docker.io/" + repo + ":runner-ci-linux-amd64"
		_, desc, err := resolver.Resolve(ctx, ref)
		if err != nil {
			if strings.Contains(err.Error(), "401") {
				t.Fatalf("auth.docker.io rejected pull-scope creds: %v", err)
			}
			t.Fatalf("Resolve %s: %v", ref, err)
		}
		t.Logf("resolved %s -> digest=%s size=%d", ref, desc.Digest, desc.Size)
	})

	t.Run("push", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Pusher requires push scope from auth.docker.io. If our prod 401
		// is reproducible, this will hit the same error.
		ref := "docker.io/" + repo + ":auth-probe-do-not-pull"
		pusher, err := resolver.Pusher(ctx, ref)
		if err != nil {
			if strings.Contains(err.Error(), "401") {
				t.Fatalf("auth.docker.io rejected push-scope creds: %v", err)
			}
			t.Fatalf("Pusher %s: %v", ref, err)
		}
		// Driving a tiny manifest write forces the push-scope auth
		// roundtrip. We don't care if the upload finalizes — we just want
		// to see whether the auth flow returns 401.
		w, err := pusher.Push(ctx, manifestlikeDesc())
		if err != nil {
			if strings.Contains(err.Error(), "401") {
				t.Fatalf("auth.docker.io rejected push-scope creds at upload: %v", err)
			}
			t.Logf("pusher.Push returned non-401 error (likely benign — we never finalize): %v", err)
			return
		}
		_ = w.Close()
	})
}

// manifestlikeDesc builds a fake descriptor we can hand to Pusher.Push
// just to drive the auth flow. The mock content is never finalized.
func manifestlikeDesc() ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Size:      0,
	}
}
