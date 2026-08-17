package pkgcache

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// ArtifactRoute is the URL prefix every proxy serves immutable package
// artifacts under. Metadata documents (npm packuments, PEP 503 index pages,
// pub version listings) carry ABSOLUTE download URLs pointing at the
// upstream CDN; left alone, the client would fetch the bytes — which are the
// overwhelming bulk of a CI download — straight from the origin and the
// cache would only ever hold metadata. Each proxy therefore rewrites those
// URLs to this route, encoding the original URL in the path.
//
// The prefix is namespaced under "_ephemerd" so it cannot collide with a
// real package name in any of the three ecosystems: npm package names may
// not begin with an underscore, PEP 503 project names are normalised to
// [a-z0-9-], and pub package names are [a-z0-9_] but are only ever served
// under /api/.
const ArtifactRoute = "/_ephemerd/dl"

// HealthRoute is the unauthenticated liveness endpoint every proxy serves.
// main.go probes it before injecting the proxy's env vars into job
// containers — see the fail-open discussion in each proxy's package doc.
const HealthRoute = "/_ephemerd/healthz"

// metadataSuffix is the PEP 658 convention: pip derives the URL of a wheel's
// standalone METADATA file by appending this to the wheel's own URL. Because
// pip appends it to the URL WE advertised, the suffix arrives on the
// cosmetic filename segment of an artifact path rather than inside the
// encoded upstream URL. ParseArtifactPath re-attaches it upstream so the
// optimisation keeps working through the cache instead of silently serving
// a whole wheel where pip expects a metadata file.
const metadataSuffix = ".metadata"

// SafeSegments splits a URL path into segments and rejects anything that
// could escape a cache root or confuse the host filesystem: traversal,
// empty segments, absolute/UNC forms, backslashes, drive letters, control
// characters, and Windows-reserved punctuation.
//
// This is the FIRST of two independent traversal defences; Cache.KeyPath
// applies a containment check on the resolved path as the second. Issue #129
// notes the Go module proxy's equivalent is untested — every branch here is
// covered by TestSafeSegments and the per-proxy traversal tests.
func SafeSegments(urlPath string) ([]string, error) {
	clean := strings.Trim(urlPath, "/")
	if clean == "" {
		return nil, fmt.Errorf("empty path")
	}
	segs := strings.Split(clean, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if s == "" {
			return nil, fmt.Errorf("empty path segment in %q", urlPath)
		}
		if s == "." || s == ".." {
			return nil, fmt.Errorf("path traversal segment %q in %q", s, urlPath)
		}
		if strings.ContainsAny(s, `\:*?"<>|`) {
			return nil, fmt.Errorf("illegal character in path segment %q", s)
		}
		// A trailing dot or space is stripped by the Win32 path parser, so
		// "foo." and "foo" would collide on Windows and differ elsewhere.
		if strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
			return nil, fmt.Errorf("path segment %q ends in a dot or space", s)
		}
		for _, r := range s {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("control character in path segment %q", s)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// ArtifactURL builds the proxy-side URL a client should use to download the
// artifact at upstreamURL. base is the proxy's advertised origin
// ("http://10.88.0.1:8084").
//
// The upstream URL is base64url-encoded (no padding, so no "=" to escape)
// into one path segment, and the artifact's real filename is appended as a
// second, purely cosmetic segment. The filename matters to pip, which parses
// the distribution name, version and wheel tags out of the LAST path segment
// of a download URL — an opaque hash there would make every wheel
// unresolvable.
func ArtifactURL(base, upstreamURL string) string {
	enc := base64.RawURLEncoding.EncodeToString([]byte(upstreamURL))
	name := artifactFilename(upstreamURL)
	return strings.TrimRight(base, "/") + ArtifactRoute + "/" + enc + "/" + name
}

// artifactFilename extracts a filesystem- and URL-safe last segment from an
// upstream URL, for use as the cosmetic filename of an artifact path.
func artifactFilename(upstreamURL string) string {
	name := upstreamURL
	if u, err := url.Parse(upstreamURL); err == nil {
		name = u.Path
	}
	name = path.Base(strings.TrimRight(name, "/"))
	name = strings.TrimLeft(name, ".")
	// Keep only characters that survive a URL path segment and a filesystem
	// name untouched. Anything else is dropped rather than escaped: this
	// value is decoration, never an identity.
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_', r == '+', r == '~':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "artifact"
	}
	return b.String()
}

// ParseArtifactPath decodes an ArtifactRoute request path back into the
// upstream URL it stands for.
//
// The returned URL is NOT trusted: the caller must still check it against a
// HostAllowlist. Encoding the upstream URL in a client-controllable path
// would otherwise turn every proxy into an open relay — a job could ask for
// http://169.254.169.254/… and read cloud metadata with the daemon's
// network identity.
func ParseArtifactPath(urlPath string) (string, error) {
	rest := strings.TrimPrefix(urlPath, ArtifactRoute+"/")
	if rest == urlPath {
		return "", fmt.Errorf("path %q is not under %s", urlPath, ArtifactRoute)
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("malformed artifact path %q", urlPath)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("decoding artifact path %q: %w", urlPath, err)
	}
	target := string(raw)
	if strings.ContainsAny(target, "\x00\r\n") {
		return "", fmt.Errorf("control character in encoded artifact URL")
	}
	// PEP 658: pip asks for "<the URL we advertised>.metadata". The suffix
	// lands on the cosmetic filename, so move it onto the upstream URL.
	if strings.HasSuffix(parts[1], metadataSuffix) && !strings.HasSuffix(target, metadataSuffix) {
		target += metadataSuffix
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("parsing encoded artifact URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("artifact URL scheme %q is not http(s)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("artifact URL has no host")
	}
	return u.String(), nil
}

// ArtifactKey is the cache key for an upstream artifact URL. Artifacts are
// keyed by the hash of their URL rather than by its path, so an upstream
// that serves two different files at colliding paths (or a path we could
// not safely map to disk) can never collide or escape. Sharded two levels
// deep to keep directory sizes sane on filesystems that dislike very wide
// directories.
func ArtifactKey(upstreamURL string) string {
	sum := sha256.Sum256([]byte(upstreamURL))
	h := hex.EncodeToString(sum[:])
	return "dl/" + h[0:2] + "/" + h[2:4] + "/" + h
}

// HostAllowlist is the set of hosts a proxy is willing to fetch artifacts
// from. It is the SSRF fence around the ArtifactRoute: without it, the
// encoded-URL scheme would let any job reach any host the daemon can.
//
// An entry matches the host exactly, or as a parent domain (an entry of
// "pythonhosted.org" matches "files.pythonhosted.org" but not
// "evilpythonhosted.org"). Ports are ignored — the fence is about which
// machine is reached, not which port on it.
type HostAllowlist []string

// Allows reports whether rawURL may be fetched. Anything that is not
// http(s), has no host, or whose host is not covered by the list is denied.
func (h HostAllowlist) Allows(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	for _, allowed := range h {
		a := strings.ToLower(strings.TrimSpace(allowed))
		if a == "" {
			continue
		}
		// Tolerate operators pasting a URL or a host:port.
		if strings.Contains(a, "/") {
			if pu, err := url.Parse(a); err == nil && pu.Hostname() != "" {
				a = strings.ToLower(pu.Hostname())
			}
		} else if i := strings.LastIndex(a, ":"); i > 0 && !strings.Contains(a, "]") {
			a = a[:i]
		}
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// WithHostsOf returns the allowlist extended with the hosts of the given
// URLs, so a configured upstream override is always permitted to serve its
// own artifacts without the operator having to restate it.
func (h HostAllowlist) WithHostsOf(urls ...string) HostAllowlist {
	out := append(HostAllowlist{}, h...)
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if !out.Allows(u.Scheme + "://" + host) {
			out = append(out, host)
		}
	}
	return out
}
