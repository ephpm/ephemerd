package pkgcache

import (
	"encoding/base64"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeSegments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		path    string
		want    []string
		wantErr bool
	}{
		{name: "simple", path: "/simple/requests/", want: []string{"simple", "requests"}},
		{name: "scoped npm name", path: "/packument/full/@types/node", want: []string{"packument", "full", "@types", "node"}},
		{name: "no leading slash", path: "a/b", want: []string{"a", "b"}},

		{name: "empty", path: "", wantErr: true},
		{name: "root only", path: "/", wantErr: true},
		{name: "parent traversal", path: "/a/../../etc/passwd", wantErr: true},
		{name: "bare parent", path: "..", wantErr: true},
		{name: "dot segment", path: "/a/./b", wantErr: true},
		{name: "double slash", path: "/a//b", wantErr: true},
		{name: "backslash traversal", path: `/a/..\..\windows`, wantErr: true},
		{name: "drive letter", path: "/C:/windows/system32", wantErr: true},
		{name: "nul byte", path: "/a/\x00/b", wantErr: true},
		{name: "newline", path: "/a/b\nc", wantErr: true},
		{name: "trailing dot", path: "/a/b./c", wantErr: true},
		{name: "trailing space", path: "/a/b /c", wantErr: true},
		{name: "wildcard", path: "/a/*/c", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := SafeSegments(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SafeSegments(%q) = %v, want error", tc.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SafeSegments(%q): %v", tc.path, err)
			}
			if strings.Join(got, "/") != strings.Join(tc.want, "/") {
				t.Errorf("SafeSegments(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestKeyPathNeverEscapesRoot is the traversal regression test issue #129
// asks for: whatever a hostile URL decodes to, the resolved path must stay
// under the cache root or be rejected outright.
func TestKeyPathNeverEscapesRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := newTestCache(t, root, 0)

	hostile := []string{
		"../escaped",
		"../../escaped",
		"a/../../escaped",
		"/../escaped",
		"a/b/../../../escaped",
		`..\escaped`,
		`a\..\..\escaped`,
		"/a/./../../escaped",
		"C:/Windows/System32/config/SAM",
		"//server/share/file",
		"a/\x00/b",
		"....//escaped",
		"a/..%2f..%2fescaped", // pre-decoded form; %2f is literal here
	}
	for _, key := range hostile {
		got, err := c.KeyPath(key)
		if err != nil {
			continue // rejected: the desired outcome
		}
		if !isUnder(got, c.Root()) {
			t.Errorf("KeyPath(%q) = %q, which escapes root %q", key, got, c.Root())
		}
	}
}

func TestKeyPathAcceptsRealKeys(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, t.TempDir(), 0)
	for _, key := range []string{
		"packument/abbrev/express",
		"packument/full/@types/node",
		"tarball/express/express-4.18.2.tgz",
		"simple/html/requests",
		"listing/http",
		"dl/ab/cd/" + strings.Repeat("f", 64),
	} {
		got, err := c.KeyPath(key)
		if err != nil {
			t.Fatalf("KeyPath(%q): %v", key, err)
		}
		if !isUnder(got, c.Root()) {
			t.Errorf("KeyPath(%q) = %q, outside root", key, got)
		}
		if !strings.HasPrefix(got, c.Root()+string(filepath.Separator)) {
			t.Errorf("KeyPath(%q) = %q, want a path inside %q", key, got, c.Root())
		}
	}
}

func TestKeyPathRejectsReservedSuffixes(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, t.TempDir(), 0)
	// A key ending in the sidecar extension could overwrite another
	// entry's metadata; a key using the temp prefix would be swept at
	// startup.
	for _, key := range []string{"tarball/x" + metaExt, tmpPrefix + "sneaky"} {
		if _, err := c.KeyPath(key); err == nil {
			t.Errorf("KeyPath(%q) = nil error, want rejection", key)
		}
	}
}

func TestArtifactURLRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []string{
		"https://registry.npmjs.org/express/-/express-4.18.2.tgz",
		"https://files.pythonhosted.org/packages/aa/bb/requests-2.31.0-py3-none-any.whl",
		"https://pub.dev/api/archives/http-1.2.0.tar.gz",
		"https://example.test/a%20b/c?d=e&f=g",
	}
	for _, upstream := range tests {
		full := ArtifactURL("http://10.88.0.1:8084", upstream)
		u, err := url.Parse(full)
		if err != nil {
			t.Fatalf("ArtifactURL produced an unparseable URL %q: %v", full, err)
		}
		got, err := ParseArtifactPath(u.Path)
		if err != nil {
			t.Fatalf("ParseArtifactPath(%q): %v", u.Path, err)
		}
		if got != upstream {
			t.Errorf("round trip = %q, want %q", got, upstream)
		}
	}
}

// TestArtifactURLKeepsFilename pins the property pip depends on: the LAST
// path segment must still be the distribution filename, because pip parses
// the project, version and wheel tags out of it.
func TestArtifactURLKeepsFilename(t *testing.T) {
	t.Parallel()
	full := ArtifactURL("http://gw:8085", "https://files.pythonhosted.org/packages/aa/bb/requests-2.31.0-py3-none-any.whl")
	if !strings.HasSuffix(full, "/requests-2.31.0-py3-none-any.whl") {
		t.Errorf("ArtifactURL = %q, want it to end in the wheel filename", full)
	}
}

// TestArtifactPathMetadataSuffix covers PEP 658: pip asks for
// "<advertised URL>.metadata", and the suffix must move back onto the
// UPSTREAM URL rather than silently serving the whole wheel.
func TestArtifactPathMetadataSuffix(t *testing.T) {
	t.Parallel()
	upstream := "https://files.pythonhosted.org/packages/aa/bb/requests-2.31.0-py3-none-any.whl"
	full := ArtifactURL("http://gw:8085", upstream)
	u, err := url.Parse(full + metadataSuffix)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseArtifactPath(u.Path)
	if err != nil {
		t.Fatalf("ParseArtifactPath: %v", err)
	}
	if got != upstream+metadataSuffix {
		t.Errorf("metadata URL = %q, want %q", got, upstream+metadataSuffix)
	}
}

func TestParseArtifactPathRejects(t *testing.T) {
	t.Parallel()
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	tests := []struct {
		name string
		path string
	}{
		{"not the artifact route", "/simple/requests/"},
		{"no filename segment", ArtifactRoute + "/" + enc("https://a.test/x")},
		{"undecodable", ArtifactRoute + "/!!!!/x"},
		{"file scheme", ArtifactRoute + "/" + enc("file:///etc/passwd") + "/passwd"},
		{"gopher scheme", ArtifactRoute + "/" + enc("gopher://a.test/x") + "/x"},
		{"no host", ArtifactRoute + "/" + enc("https:///etc/passwd") + "/passwd"},
		{"header injection", ArtifactRoute + "/" + enc("https://a.test/x\r\nX: y") + "/x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := ParseArtifactPath(tc.path); err == nil {
				t.Errorf("ParseArtifactPath(%q) = %q, want error", tc.path, got)
			}
		})
	}
}

func TestArtifactKeyIsStableAndSafe(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, t.TempDir(), 0)
	// Even a URL built entirely out of traversal must produce a key that
	// resolves inside the cache root — the key is a hash, not a path.
	for _, u := range []string{
		"https://a.test/../../../../etc/passwd",
		"https://a.test/" + strings.Repeat("../", 40) + "etc/shadow",
	} {
		key := ArtifactKey(u)
		p, err := c.KeyPath(key)
		if err != nil {
			t.Fatalf("KeyPath(ArtifactKey(%q)): %v", u, err)
		}
		if !isUnder(p, c.Root()) {
			t.Errorf("artifact key for %q escaped the root: %q", u, p)
		}
	}
	if ArtifactKey("https://a.test/x") != ArtifactKey("https://a.test/x") {
		t.Error("ArtifactKey is not deterministic")
	}
	if ArtifactKey("https://a.test/x") == ArtifactKey("https://a.test/y") {
		t.Error("ArtifactKey collided for different URLs")
	}
}

func TestHostAllowlist(t *testing.T) {
	t.Parallel()
	allow := HostAllowlist{"registry.npmjs.org", "pythonhosted.org"}
	allowed := []string{
		"https://registry.npmjs.org/x/-/x-1.tgz",
		"http://registry.npmjs.org/x",
		"https://files.pythonhosted.org/packages/x.whl",
		"https://REGISTRY.NPMJS.ORG/x",
		"https://registry.npmjs.org:443/x",
	}
	denied := []string{
		// The SSRF cases this fence exists for.
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:10000/",
		"http://10.88.0.1:8082/",
		"file:///etc/passwd",
		"gopher://registry.npmjs.org/",
		// Lookalikes that a naive suffix match would let through.
		"https://evilpythonhosted.org/x.whl",
		"https://registry.npmjs.org.evil.test/x",
		"https://notregistry.npmjs.org.evil/x",
		"https://",
	}
	for _, u := range allowed {
		if !allow.Allows(u) {
			t.Errorf("Allows(%q) = false, want true", u)
		}
	}
	for _, u := range denied {
		if allow.Allows(u) {
			t.Errorf("Allows(%q) = true, want false", u)
		}
	}
}

func TestHostAllowlistWithHostsOf(t *testing.T) {
	t.Parallel()
	allow := HostAllowlist{"registry.npmjs.org"}.WithHostsOf("http://mirror.internal.test:4873")
	if !allow.Allows("http://mirror.internal.test:4873/x/-/x-1.tgz") {
		t.Error("a configured upstream must be allowed to serve its own artifacts")
	}
	if allow.Allows("http://other.internal.test/x") {
		t.Error("WithHostsOf widened the allowlist beyond the upstream host")
	}
}

func TestArtifactFilenameSanitises(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"https://a.test/pkg/../../etc/passwd": "passwd",
		"https://a.test/a/b/":                 "b",
		"https://a.test/":                     "artifact",
		"https://a.test/..":                   "artifact",
		`https://a.test/a\b`:                  "ab",
		"https://a.test/x?y=z":                "x",
	}
	for in, want := range tests {
		if got := artifactFilename(in); got != want {
			t.Errorf("artifactFilename(%q) = %q, want %q", in, got, want)
		}
	}
	// Whatever comes out must be a single safe path segment.
	for in := range tests {
		if _, err := SafeSegments("/" + artifactFilename(in)); err != nil {
			t.Errorf("artifactFilename(%q) produced an unsafe segment: %v", in, err)
		}
	}
}
