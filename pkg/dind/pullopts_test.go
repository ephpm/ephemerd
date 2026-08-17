package dind

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/registrymirror"
)

// newPullOptsServer builds the minimum Server the pull paths need: an auth
// cache, a logger, and an optional mirror. No containerd, no socket.
func newPullOptsServer(mirror *registrymirror.Mirror) *Server {
	return &Server{
		jobID:  "test-job",
		mirror: mirror,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// authRequest builds a request carrying an X-Registry-Auth header, the way
// docker CLI sends credentials on a pull.
func authRequest(t *testing.T, cfg authConfig) *http.Request {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/images/create?fromImage=alpine&tag=3.20", nil)
	r.Header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(raw))
	return r
}

// TestPullRemoteOpts is the no-regression guarantee for the dind pull paths.
// The table's first row is the one that matters most: an unconfigured node
// with an anonymous pull must add nothing at all, leaving client.Pull the
// exact call it was before registry mirroring existed.
func TestPullRemoteOpts(t *testing.T) {
	mirror := registrymirror.New(config.RegistryMirrorConfig{
		Enabled:  true,
		Endpoint: "http://cache.example.com:5000",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if mirror == nil {
		t.Fatal("test fixture did not produce a mirror")
	}

	tests := []struct {
		name     string
		mirror   *registrymirror.Mirror
		req      func(t *testing.T) *http.Request
		cached   *authConfig // credentials to seed the per-job login cache with
		wantOpts int
	}{
		{
			name:   "no mirror, anonymous pull adds nothing",
			mirror: nil,
			req: func(*testing.T) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/images/create?fromImage=alpine", nil)
			},
			wantOpts: 0,
		},
		{
			name:   "no mirror, empty auth header is still anonymous",
			mirror: nil,
			// Docker CLI sends `{}` base64-encoded on anonymous pulls; it
			// decodes cleanly and must not be mistaken for a login.
			req:      func(t *testing.T) *http.Request { return authRequest(t, authConfig{}) },
			wantOpts: 0,
		},
		{
			name:     "mirror configured, anonymous pull gets a resolver",
			mirror:   mirror,
			req:      func(*testing.T) *http.Request { return httptest.NewRequest(http.MethodPost, "/images/create", nil) },
			wantOpts: 1,
		},
		{
			// Issue #139: handleImagePull passed no authorizer, so a
			// `docker login` followed by a private pull went out anonymously
			// and 401'd, even on a node with no mirror at all.
			name:   "no mirror, header credentials still produce an authorizer",
			mirror: nil,
			req: func(t *testing.T) *http.Request {
				return authRequest(t, authConfig{Username: "u", Password: "dckr_pat_secret"})
			},
			wantOpts: 1,
		},
		{
			name:   "no mirror, cached docker login credentials produce an authorizer",
			mirror: nil,
			req: func(*testing.T) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/images/create", nil)
			},
			cached:   &authConfig{Username: "u", Password: "dckr_pat_secret", Serveraddress: "docker.io"},
			wantOpts: 1,
		},
		{
			name:   "no mirror, an identity token counts as credentials",
			mirror: nil,
			req: func(t *testing.T) *http.Request {
				return authRequest(t, authConfig{IdentityToken: "identity-token"})
			},
			wantOpts: 1,
		},
		{
			name:   "mirror plus credentials still adds exactly one resolver",
			mirror: mirror,
			req: func(t *testing.T) *http.Request {
				return authRequest(t, authConfig{Username: "u", Password: "dckr_pat_secret"})
			},
			wantOpts: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newPullOptsServer(tt.mirror)
			if tt.cached != nil {
				s.auth.put(tt.cached.Serveraddress, *tt.cached)
			}
			got := s.pullRemoteOpts(tt.req(t), "docker.io/library/alpine:3.20")
			if len(got) != tt.wantOpts {
				t.Errorf("pullRemoteOpts() returned %d opts, want %d", len(got), tt.wantOpts)
			}
		})
	}
}

// TestPullRemoteOpts_CredentialsAreScopedToTheirRegistry confirms the #139 fix
// does not start shipping one registry's credentials to another: the cache is
// keyed by registry host, and a login to ghcr.io must leave a docker.io pull
// anonymous.
func TestPullRemoteOpts_CredentialsAreScopedToTheirRegistry(t *testing.T) {
	s := newPullOptsServer(nil)
	s.auth.put("ghcr.io", authConfig{Username: "u", Password: "ghp_secret"})

	req := httptest.NewRequest(http.MethodPost, "/images/create", nil)

	if got := s.pullRemoteOpts(req, "docker.io/library/alpine:3.20"); len(got) != 0 {
		t.Errorf("a ghcr.io login leaked into a docker.io pull (%d opts)", len(got))
	}
	if got := s.pullRemoteOpts(req, "ghcr.io/org/img:1"); len(got) != 1 {
		t.Errorf("the ghcr.io login was not used for a ghcr.io pull (%d opts)", len(got))
	}
}

// TestAuthConfigHasSecret pins the "is this actually a login" test that keeps
// docker CLI's empty `{}` auth header from being treated as credentials.
func TestAuthConfigHasSecret(t *testing.T) {
	tests := []struct {
		name string
		cfg  authConfig
		want bool
	}{
		{"empty", authConfig{}, false},
		{"username only", authConfig{Username: "u"}, false},
		{"password", authConfig{Username: "u", Password: "p"}, true},
		{"identity token", authConfig{IdentityToken: "t"}, true},
	}
	for _, tt := range tests {
		if got := tt.cfg.hasSecret(); got != tt.want {
			t.Errorf("%s: hasSecret() = %t, want %t", tt.name, got, tt.want)
		}
	}
}
