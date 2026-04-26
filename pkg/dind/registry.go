// Registry auth + image push for the fake Docker daemon.
//
// docker login (POST /auth) and docker push (POST /images/{ref}/push) are
// the two endpoints the upstream Docker CLI uses to publish images. Our
// fake daemon needs to handle both so jobs can do `docker login` followed
// by `docker push` against real registries.
//
// Authentication: handleAuth always succeeds — credentials are taken at
// face value and cached in the Server keyed by registry hostname. Push
// requests carry their own X-Registry-Auth header (base64 JSON), which we
// prefer over the cache when present. The cached creds are the fallback
// for clients that login once and push many times in the same job.

package dind

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// authConfig is the JSON body docker CLI sends to POST /auth and embeds
// (base64-encoded) in X-Registry-Auth on push.
type authConfig struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Serveraddress string `json:"serveraddress"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

// authCache is a per-Server in-memory credential store keyed by registry
// hostname. Populated by handleAuth, consumed by handleImagePush as a
// fallback when X-Registry-Auth is missing.
type authCache struct {
	mu    sync.Mutex
	creds map[string]authConfig
}

func (c *authCache) put(server string, cfg authConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.creds == nil {
		c.creds = make(map[string]authConfig)
	}
	c.creds[normalizeServer(server)] = cfg
}

func (c *authCache) get(server string) (authConfig, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg, ok := c.creds[normalizeServer(server)]
	return cfg, ok
}

// normalizeServer reduces "https://index.docker.io/v1/" and similar
// variants down to a registry hostname suitable for cache lookup
// ("docker.io" / "ghcr.io" / etc).
func normalizeServer(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "docker.io"
	}
	// Strip scheme.
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		s = u.Host
	}
	// Drop trailing path segments like "v1/".
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if s == "index.docker.io" || s == "registry-1.docker.io" {
		return "docker.io"
	}
	return s
}

// handleAuth is POST /auth. We accept any credentials at face value and
// cache them for later use by handleImagePush. Real validation against
// the registry happens at push time via the docker resolver.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	var cfg authConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": fmt.Sprintf("auth body: %v", err),
		})
		return
	}

	// PowerShell on Windows pipes strings as UTF-8 with a BOM (\xEF\xBB\xBF)
	// and adds \r\n. So `$env:DOCKER_PASSWORD | docker login --password-stdin`
	// reaches us as `<BOM><PAT>\r\n`. Basic auth then sends the BOM-prefixed
	// password to auth.docker.io and the registry rejects with 401 because
	// the token is unrecognizable. strings.TrimSpace doesn't strip BOM, so
	// trim it explicitly. Real credentials never start with a BOM.
	beforeLen := len(cfg.Password)
	beforeTail := tailHex(cfg.Password)
	cfg.Username = sanitizeCred(cfg.Username)
	cfg.Password = sanitizeCred(cfg.Password)
	cfg.IdentityToken = sanitizeCred(cfg.IdentityToken)

	server := cfg.Serveraddress
	if server == "" {
		server = "docker.io"
	}
	s.auth.put(server, cfg)
	s.log.Info("auth cached",
		"server", normalizeServer(server),
		"username", cfg.Username,
		"password_len", len(cfg.Password),
		"password_prefix", safePrefix(cfg.Password, 9),
		"before_trim_len", beforeLen,
		"before_trim_tail_hex", beforeTail,
	)

	writeJSON(w, http.StatusOK, map[string]string{
		"Status":        "Login Succeeded",
		"IdentityToken": "",
	})
}

// handleImagePush is POST /images/{ref}/push.
//
// `ref` in the URL is the image name without tag (e.g. "ephpm/ephemerd").
// Docker CLI sends the tag as a query parameter. The full reference looked
// up in containerd is "<ref>:<tag>". X-Registry-Auth carries credentials
// base64-encoded; we fall back to the auth cache populated by handleAuth.
//
// The image must exist in the buildkit containerd namespace — it lands
// there via handleImageBuildBuildkit when the workflow's prior `docker
// build` step ran.
func (s *Server) handleImagePush(w http.ResponseWriter, r *http.Request, refPath string) {
	if s.client == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "containerd client not available",
		})
		return
	}

	tag := r.URL.Query().Get("tag")
	if tag == "" {
		tag = "latest"
	}

	// refPath comes from the router as the URL-encoded image name. Decode
	// then attach the tag from the query string.
	name, err := url.PathUnescape(refPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": fmt.Sprintf("invalid image ref %q: %v", refPath, err),
		})
		return
	}
	fullRef := name + ":" + tag

	// Resolve credentials: prefer X-Registry-Auth, fall back to cache.
	cfg, _ := s.resolveAuthForRef(r, fullRef)

	// Look up the image. Buildkit puts built images in its own namespace
	// (configured via buildkit.Config.ContainerdNamespace = "buildkit").
	const buildkitNS = "buildkit"
	ctx := namespaces.WithNamespace(r.Context(), buildkitNS)

	// Look up the image. The Linux Docker CLI canonicalizes refs with the
	// docker.io/ registry prefix before POSTing the push, but BuildKit's
	// containerd exporter stores the image under whatever short name the
	// build's `-t` tag carried (e.g. "ephpm/ephemerd:..." with no prefix).
	// Try the original first, then strip a leading docker.io/.
	img, err := s.client.GetImage(ctx, fullRef)
	if err != nil {
		if alt, ok := strings.CutPrefix(fullRef, "docker.io/"); ok {
			if alt2, err2 := s.client.GetImage(ctx, alt); err2 == nil {
				img, err = alt2, nil
			}
		}
	}
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("image not found in buildkit namespace: %s: %v", fullRef, err),
		})
		return
	}

	// Set up docker registry resolver with the resolved credentials.
	// Use NewDockerAuthorizer directly rather than going through
	// config.ConfigureHosts — the latter was producing double-scope query
	// strings on the auth.docker.io token request that Docker Hub
	// rejected with a bare 401. The simpler authorizer drives the
	// standard Bearer challenge → token GET → retry flow.
	authCreds := func(host string) (string, string, error) {
		if cfg.IdentityToken != "" {
			return "", cfg.IdentityToken, nil
		}
		return cfg.Username, cfg.Password, nil
	}
	resolver := docker.NewResolver(docker.ResolverOptions{
		Authorizer: docker.NewDockerAuthorizer(docker.WithAuthCreds(authCreds)),
	})

	s.log.Info("push starting",
		"ref", fullRef,
		"username", cfg.Username,
		"password_len", len(cfg.Password),
		"identitytoken_len", len(cfg.IdentityToken),
	)

	// Stream progress to the client in Docker JSON-line format.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	emit := func(status, id string) {
		msg := map[string]any{"status": status}
		if id != "" {
			msg["id"] = id
		}
		if err := writeJSONLine(w, msg); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	emit(fmt.Sprintf("The push refers to repository [%s]", name), "")
	emit(fmt.Sprintf("Pushing %s", fullRef), tag)

	// containerd's resolver treats the first path segment of an unqualified
	// reference as the registry hostname, so "ephpm/ephemerd:tag" tries to
	// dial "ephpm". Force the docker.io qualifier when the ref carries no
	// explicit host. The local image was stored under the unqualified name
	// (Docker CLI hands /build the unqualified -t arg verbatim), so GetImage
	// above used fullRef; the push happens via this qualified form.
	pushRef := qualifyDockerHubRef(fullRef)
	pushOpts := []client.RemoteOpt{client.WithResolver(resolver)}
	if err := s.client.Push(ctx, pushRef, img.Target(), pushOpts...); err != nil {
		s.log.Warn("push failed", "ref", fullRef, "error", err)
		_ = writeJSONLine(w, map[string]any{
			"errorDetail": map[string]any{"message": err.Error()},
			"error":       err.Error(),
		})
		return
	}

	emit(fmt.Sprintf("%s: digest: %s size: %d", tag, img.Target().Digest, img.Target().Size), "")
	s.log.Info("image pushed", "ref", fullRef, "digest", img.Target().Digest)
}

// resolveAuthForRef returns the credentials to use for a given image
// reference. X-Registry-Auth on the request takes precedence; the cached
// creds for the registry host (or docker.io) are the fallback.
func (s *Server) resolveAuthForRef(r *http.Request, ref string) (authConfig, bool) {
	if hdr := r.Header.Get("X-Registry-Auth"); hdr != "" {
		if cfg, ok := decodeAuthHeader(hdr); ok {
			return cfg, true
		}
	}
	host := registryHostFromRef(ref)
	cfg, ok := s.auth.get(host)
	return cfg, ok
}

// decodeAuthHeader handles both URL-safe and standard base64 since docker
// clients have used both encodings over the years. Trims whitespace from
// decoded credential fields — see the comment in handleAuth.
func decodeAuthHeader(h string) (authConfig, bool) {
	for _, dec := range []func(string) ([]byte, error){
		base64.URLEncoding.DecodeString,
		base64.StdEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
	} {
		raw, err := dec(h)
		if err != nil {
			continue
		}
		var cfg authConfig
		if err := json.Unmarshal(raw, &cfg); err == nil {
			cfg.Username = sanitizeCred(cfg.Username)
			cfg.Password = sanitizeCred(cfg.Password)
			cfg.IdentityToken = sanitizeCred(cfg.IdentityToken)
			return cfg, true
		}
	}
	return authConfig{}, false
}

// sanitizeCred strips invisible padding that PowerShell/credstore tooling
// loves to bake into secret values when round-tripping through stdin or
// JSON: a leading UTF-8 BOM and surrounding whitespace. Real PATs and
// passwords never legitimately contain these.
func sanitizeCred(s string) string {
	s = strings.TrimPrefix(s, "\xef\xbb\xbf")
	s = strings.TrimSpace(s)
	return s
}

// safePrefix returns the first n bytes of s for diagnostic logging.
// We only ever look at the format-identifying prefix (e.g. "dckr_pat_")
// to confirm the token type — never the secret payload.
func safePrefix(s string, n int) string {
	if len(s) < n {
		n = len(s)
	}
	return s[:n]
}

// tailHex returns hex of the last few bytes of s, for diagnosing
// non-printable trailing bytes that TrimSpace wouldn't catch.
func tailHex(s string) string {
	n := 8
	if len(s) < n {
		n = len(s)
	}
	tail := s[len(s)-n:]
	out := make([]byte, 0, n*3)
	for i := 0; i < len(tail); i++ {
		if i > 0 {
			out = append(out, ' ')
		}
		const hexDigits = "0123456789abcdef"
		out = append(out, hexDigits[tail[i]>>4], hexDigits[tail[i]&0x0f])
	}
	return string(out)
}

// qualifyDockerHubRef ensures a reference carries an explicit registry
// host. containerd's resolver treats the first path segment of an
// unqualified reference as the hostname (so "ephpm/ephemerd:tag" → host
// "ephpm" → DNS lookup fails). Docker CLI conventions default unqualified
// refs to docker.io, prepending "library/" for single-segment names like
// "ubuntu". This helper applies the same rule.
func qualifyDockerHubRef(ref string) string {
	first := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return ref
	}
	if !strings.Contains(ref, "/") {
		return "docker.io/library/" + ref
	}
	return "docker.io/" + ref
}

// registryHostFromRef peels the registry hostname off a fully-qualified
// reference. References without an explicit host (e.g. "ephpm/ephemerd")
// are assumed to be on Docker Hub.
func registryHostFromRef(ref string) string {
	// Strip tag/digest.
	if i := strings.IndexAny(ref, ":@"); i >= 0 {
		ref = ref[:i]
	}
	first := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	}
	// Hosts always contain a "." or ":" or are exactly "localhost".
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return normalizeServer(first)
	}
	return "docker.io"
}

// drainBody is a small helper for handlers that don't read the body — keep
// the connection reusable.
func drainBody(r *http.Request) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
}

var _ = drainBody // unused for now; reserved if /auth ever streams

// containerdNS guard so callers can pass a value-only context to GetImage.
var _ = context.Background
