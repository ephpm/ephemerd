package registrymirror

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mustMirror builds a Mirror and fails the test if the config resolves to
// nothing, so a typo in a test fixture surfaces as a clear failure rather than
// a nil that silently exercises the no-mirror path.
func mustMirror(t *testing.T, cfg config.RegistryMirrorConfig) *Mirror {
	t.Helper()
	m := New(cfg, testLogger())
	if m == nil {
		t.Fatalf("New(%+v) = nil; expected a configured mirror", cfg)
	}
	return m
}

// TestNew_NoMirrorIsNil pins the property every call site relies on: with no
// mirror configured, New returns nil and every method is a no-op, so the pull
// paths build exactly the containerd call they built before this package
// existed.
func TestNew_NoMirrorIsNil(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.RegistryMirrorConfig
	}{
		{"zero value", config.RegistryMirrorConfig{}},
		{"configured but disabled", config.RegistryMirrorConfig{
			Endpoint: "http://cache.example.com:5000",
		}},
		{"enabled with nothing mapped", config.RegistryMirrorConfig{Enabled: true}},
		{"enabled with an unusable endpoint", config.RegistryMirrorConfig{
			Enabled:  true,
			Endpoint: "http://",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New(tt.cfg, testLogger())
			if m != nil {
				t.Fatalf("New() = %v, want nil", m)
			}
			if m.Enabled() {
				t.Error("nil Mirror reports Enabled()")
			}
			if opts := m.PullOpts(nil); opts != nil {
				t.Errorf("nil Mirror produced %d pull opts; the no-mirror pull must be untouched", len(opts))
			}
			// Must not panic on a nil receiver.
			m.LogPull("docker.io/library/alpine:3.20")
			if _, ok := m.Endpoint("docker.io"); ok {
				t.Error("nil Mirror resolved an endpoint")
			}
		})
	}
}

// TestPullOpts_CredentialsAloneStillBuildAResolver covers the #139 path: dind
// needs a resolver carrying `docker login` credentials even on a node with no
// mirror at all.
func TestPullOpts_CredentialsAloneStillBuildAResolver(t *testing.T) {
	var m *Mirror // no mirror configured
	creds := Creds(func(string) (string, string, error) { return "u", "p", nil })
	if opts := m.PullOpts(creds); len(opts) != 1 {
		t.Fatalf("PullOpts(creds) returned %d opts, want 1 (a resolver carrying the credentials)", len(opts))
	}
	if opts := m.PullOpts(nil); opts != nil {
		t.Fatalf("PullOpts(nil) returned %d opts, want none", len(opts))
	}
}

// TestRegistryHosts_Ordering is the core of the feature: for a mirrored
// registry the mirror is tried first and the origin sits behind it; for every
// other registry containerd's own answer is passed through untouched.
func TestRegistryHosts_Ordering(t *testing.T) {
	const originHost = "origin.example.com"

	// A stand-in for containerd's default endpoint table, so these cases
	// assert on ordering rather than on containerd's internals.
	origins := docker.RegistryHosts(func(registry string) ([]docker.RegistryHost, error) {
		return []docker.RegistryHost{{
			Host:         originHost,
			Scheme:       "https",
			Path:         "/v2",
			Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve | docker.HostCapabilityPush,
		}}, nil
	})

	tests := []struct {
		name      string
		cfg       config.RegistryMirrorConfig
		lookup    string
		wantHosts []string // Host field of each returned entry, in order
		wantPath  string   // Path of the first entry, when it is the mirror
	}{
		{
			name: "mirrored registry puts the cache first and keeps the origin behind it",
			cfg: config.RegistryMirrorConfig{
				Enabled:  true,
				Endpoint: "http://cache.example.com:5000",
			},
			lookup:    "docker.io",
			wantHosts: []string{"cache.example.com:5000", originHost},
			wantPath:  "/v2",
		},
		{
			name: "unmirrored registry is passed through unchanged",
			cfg: config.RegistryMirrorConfig{
				Enabled:  true,
				Endpoint: "http://cache.example.com:5000",
			},
			lookup:    "ghcr.io",
			wantHosts: []string{originHost},
		},
		{
			name: "fallback disabled makes the mirror authoritative",
			cfg: config.RegistryMirrorConfig{
				Enabled:          true,
				Endpoint:         "http://cache.example.com:5000",
				FallbackToOrigin: boolPtr(false),
			},
			lookup:    "docker.io",
			wantHosts: []string{"cache.example.com:5000"},
			wantPath:  "/v2",
		},
		{
			name: "per-registry mapping routes its own host",
			cfg: config.RegistryMirrorConfig{
				Enabled: true,
				Mirrors: map[string]string{"ghcr.io": "https://ghcr-cache.example.com"},
			},
			lookup:    "ghcr.io",
			wantHosts: []string{"ghcr-cache.example.com", originHost},
			wantPath:  "/v2",
		},
		{
			name: "per-registry mapping leaves other registries alone",
			cfg: config.RegistryMirrorConfig{
				Enabled: true,
				Mirrors: map[string]string{"ghcr.io": "https://ghcr-cache.example.com"},
			},
			lookup:    "docker.io",
			wantHosts: []string{originHost},
		},
		{
			name: "a path prefix on the endpoint is kept ahead of /v2",
			cfg: config.RegistryMirrorConfig{
				Enabled:  true,
				Endpoint: "https://harbor.example.com/dockerhub-proxy",
			},
			lookup:    "docker.io",
			wantHosts: []string{"harbor.example.com", originHost},
			wantPath:  "/dockerhub-proxy/v2",
		},
		{
			name: "docker hub's alternate spelling still finds the mirror",
			cfg: config.RegistryMirrorConfig{
				Enabled:  true,
				Endpoint: "http://cache.example.com:5000",
			},
			lookup:    "index.docker.io",
			wantHosts: []string{"cache.example.com:5000", originHost},
			wantPath:  "/v2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mustMirror(t, tt.cfg)
			hosts, err := m.registryHosts(origins, nil)(tt.lookup)
			if err != nil {
				t.Fatalf("registryHosts(%q): %v", tt.lookup, err)
			}
			got := make([]string, len(hosts))
			for i, h := range hosts {
				got[i] = h.Host
			}
			if strings.Join(got, ",") != strings.Join(tt.wantHosts, ",") {
				t.Fatalf("host order = %v, want %v", got, tt.wantHosts)
			}
			if tt.wantPath != "" && hosts[0].Path != tt.wantPath {
				t.Errorf("mirror Path = %q, want %q", hosts[0].Path, tt.wantPath)
			}
		})
	}
}

// TestRegistryHosts_MirrorIsPullOnly guards the one capability rule that
// matters: a pull-through cache must never be offered as a push target, or a
// job's `docker push` would publish into the cache instead of the registry.
func TestRegistryHosts_MirrorIsPullOnly(t *testing.T) {
	m := mustMirror(t, config.RegistryMirrorConfig{
		Enabled:  true,
		Endpoint: "http://cache.example.com:5000",
	})
	hosts, err := m.RegistryHosts(nil)("docker.io")
	if err != nil {
		t.Fatal(err)
	}
	mirrorHost := hosts[0]
	if !mirrorHost.Capabilities.Has(docker.HostCapabilityPull) {
		t.Error("mirror cannot pull")
	}
	if !mirrorHost.Capabilities.Has(docker.HostCapabilityResolve) {
		t.Error("mirror cannot resolve tags")
	}
	if mirrorHost.Capabilities.Has(docker.HostCapabilityPush) {
		t.Error("mirror advertises push; a pull-through cache must never be a push target")
	}
	if mirrorHost.Capabilities.Has(docker.HostCapabilityReferrers) {
		t.Error("mirror advertises the referrers API; that just 404s once per pull")
	}
}

// TestRegistryHosts_CredentialsStayOffTheMirror is the credential-leak guard.
// A cache that answered with a Basic challenge would otherwise harvest the
// registry PAT a job just logged in with — in plaintext when the endpoint is
// http://. The origin keeps its authorizer either way.
func TestRegistryHosts_CredentialsStayOffTheMirror(t *testing.T) {
	creds := Creds(func(string) (string, string, error) { return "user", "dckr_pat_secret", nil })

	withoutForwarding := mustMirror(t, config.RegistryMirrorConfig{
		Enabled:  true,
		Endpoint: "http://cache.example.com:5000",
	})
	hosts, err := withoutForwarding.RegistryHosts(creds)("docker.io")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("got %d hosts, want mirror + origin", len(hosts))
	}
	if hosts[0].Authorizer == hosts[1].Authorizer {
		t.Error("mirror shares the origin's credentialed authorizer; forward_credentials is off")
	}
	if hosts[1].Authorizer == nil {
		t.Error("origin lost its authorizer; anonymous Docker Hub pulls need one for the token flow")
	}

	withForwarding := mustMirror(t, config.RegistryMirrorConfig{
		Enabled:            true,
		Endpoint:           "http://cache.example.com:5000",
		ForwardCredentials: true,
	})
	hosts, err = withForwarding.RegistryHosts(creds)("docker.io")
	if err != nil {
		t.Fatal(err)
	}
	if hosts[0].Authorizer != hosts[1].Authorizer {
		t.Error("forward_credentials = true did not give the mirror the credentialed authorizer")
	}
}

// TestRegistryHosts_NilMirrorYieldsContainerdDefaults confirms the nil path
// produces containerd's stock endpoint table, which is what makes this usable
// as the single "pull with credentials, no mirror" code path too.
func TestRegistryHosts_NilMirrorYieldsContainerdDefaults(t *testing.T) {
	var m *Mirror
	hosts, err := m.RegistryHosts(nil)("docker.io")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts, want exactly containerd's default", len(hosts))
	}
	if hosts[0].Host != "registry-1.docker.io" {
		t.Errorf("default docker.io host = %q, want registry-1.docker.io", hosts[0].Host)
	}
	if !hosts[0].Capabilities.Has(docker.HostCapabilityPush) {
		t.Error("default host lost push capability")
	}
}

// TestResolve_FallsBackToOriginWhenMirrorIsDown is the fail-open proof, run
// against real HTTP servers rather than asserting on struct fields: the
// "mirror" is a server that fails every request, the "origin" serves a real
// manifest, and the resolve has to succeed anyway.
//
// This is the behavior a dead LAN cache depends on. It must degrade a node to
// the WAN speed it had before the mirror existed — never fail the job.
func TestResolve_FallsBackToOriginWhenMirrorIsDown(t *testing.T) {
	tests := []struct {
		name          string
		mirrorHandler http.HandlerFunc
	}{
		{
			name: "mirror returns 500",
			mirrorHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "mirror does not have the image",
			mirrorHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
		},
		{
			name: "mirror rejects with 401",
			mirrorHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mirrorHits, originHits atomic.Int32

			mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mirrorHits.Add(1)
				tt.mirrorHandler(w, r)
			}))
			defer mirrorSrv.Close()

			manifest, digest := testManifest(t)
			originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originHits.Add(1)
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
				w.Header().Set("Docker-Content-Digest", digest)
				w.Header().Set("Content-Length", fmt.Sprint(len(manifest)))
				w.WriteHeader(http.StatusOK)
				if r.Method == http.MethodGet {
					_, _ = w.Write(manifest)
				}
			}))
			defer originSrv.Close()

			m := mustMirror(t, config.RegistryMirrorConfig{
				Enabled:  true,
				Endpoint: mirrorSrv.URL,
			})

			resolver := docker.NewResolver(docker.ResolverOptions{
				Hosts: m.registryHosts(staticHosts(t, originSrv.URL), nil),
			})

			_, desc, err := resolver.Resolve(context.Background(), "docker.io/library/alpine:3.20")
			if err != nil {
				t.Fatalf("resolve failed even though the origin was healthy: %v", err)
			}
			if desc.Digest.String() != digest {
				t.Errorf("resolved digest = %s, want %s", desc.Digest, digest)
			}
			if mirrorHits.Load() == 0 {
				t.Error("the mirror was never tried; it must be the first host")
			}
			if originHits.Load() == 0 {
				t.Error("the origin was never tried; fail-open did not happen")
			}
		})
	}
}

// TestResolve_UsesMirrorWhenHealthy is the other half: a working cache serves
// the resolve and the origin is never contacted, which is where the bandwidth
// saving comes from.
func TestResolve_UsesMirrorWhenHealthy(t *testing.T) {
	var originHits atomic.Int32
	manifest, digest := testManifest(t)

	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", fmt.Sprint(len(manifest)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(manifest)
		}
	}))
	defer mirrorSrv.Close()

	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer originSrv.Close()

	m := mustMirror(t, config.RegistryMirrorConfig{
		Enabled:  true,
		Endpoint: mirrorSrv.URL,
	})
	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: m.registryHosts(staticHosts(t, originSrv.URL), nil),
	})

	_, desc, err := resolver.Resolve(context.Background(), "docker.io/library/alpine:3.20")
	if err != nil {
		t.Fatalf("resolve through a healthy mirror failed: %v", err)
	}
	if desc.Digest.String() != digest {
		t.Errorf("resolved digest = %s, want %s", desc.Digest, digest)
	}
	if originHits.Load() != 0 {
		t.Errorf("origin was contacted %d times; a healthy mirror must absorb the request", originHits.Load())
	}
}

// TestResolve_NoFallbackFailsClosed confirms fallback_to_origin = false really
// does stop at the mirror — the deliberate egress-control posture.
func TestResolve_NoFallbackFailsClosed(t *testing.T) {
	var originHits atomic.Int32

	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mirrorSrv.Close()

	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer originSrv.Close()

	m := mustMirror(t, config.RegistryMirrorConfig{
		Enabled:          true,
		Endpoint:         mirrorSrv.URL,
		FallbackToOrigin: boolPtr(false),
	})
	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: m.registryHosts(staticHosts(t, originSrv.URL), nil),
	})

	if _, _, err := resolver.Resolve(context.Background(), "docker.io/library/alpine:3.20"); err == nil {
		t.Fatal("resolve succeeded with fallback disabled and a dead mirror")
	}
	if originHits.Load() != 0 {
		t.Errorf("origin was contacted %d times despite fallback_to_origin = false", originHits.Load())
	}
}

// TestRegistryHostFromRef pins the reference→registry mapping the mirror
// lookup and the log line both use.
func TestRegistryHostFromRef(t *testing.T) {
	tests := []struct{ ref, want string }{
		{"alpine", "docker.io"},
		{"alpine:3.20", "docker.io"},
		{"alpine@sha256:aaaa", "docker.io"},
		{"library/alpine:3.20", "docker.io"},
		{"ephpm/ephpm-ci:latest", "docker.io"},
		{"docker.io/ephpm/ephpm-ci:latest", "docker.io"},
		{"index.docker.io/library/alpine:3.20", "docker.io"},
		{"ghcr.io/actions/actions-runner:latest", "ghcr.io"},
		{"registry.example.com:5000/team/img:1", "registry.example.com:5000"},
		{"localhost:5000/img:1", "localhost:5000"},
		{"localhost/img:1", "localhost"},
		// A digest whose colon must not be read as a port separator.
		{"ghcr.io/actions/actions-runner@sha256:bbbb", "ghcr.io"},
	}
	for _, tt := range tests {
		if got := RegistryHostFromRef(tt.ref); got != tt.want {
			t.Errorf("RegistryHostFromRef(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

// TestJoinV2 covers the path arithmetic for caches published under a prefix.
func TestJoinV2(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "/v2"},
		{"/", "/v2"},
		{"/dockerhub-proxy", "/dockerhub-proxy/v2"},
		{"dockerhub-proxy", "/dockerhub-proxy/v2"},
		{"/a/b/", "/a/b/v2"},
	}
	for _, tt := range tests {
		if got := joinV2(tt.in); got != tt.want {
			t.Errorf("joinV2(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// staticHosts builds an origin endpoint table pointing at a local test server,
// standing in for the real registry so fallback can be observed end to end.
func staticHosts(t *testing.T, rawURL string) docker.RegistryHosts {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing test origin %q: %v", rawURL, err)
	}
	return func(string) ([]docker.RegistryHost, error) {
		return []docker.RegistryHost{{
			Client:       http.DefaultClient,
			Host:         u.Host,
			Scheme:       u.Scheme,
			Path:         "/v2",
			Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve | docker.HostCapabilityPush,
		}}, nil
	}
}

// testManifest returns a minimal but well-formed OCI manifest and its digest,
// enough for the resolver to accept a HEAD response as a real resolution.
func testManifest(t *testing.T) ([]byte, string) {
	t.Helper()
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			Size:      2,
		},
	}
	manifest.SchemaVersion = 2
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return raw, digest.FromBytes(raw).String()
}

func boolPtr(b bool) *bool { return &b }
