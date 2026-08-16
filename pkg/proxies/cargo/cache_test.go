package cargoproxy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecideFreshness(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	ttl := 10 * time.Minute

	tests := []struct {
		name      string
		cached    bool
		immutable bool
		fetched   time.Time
		ttl       time.Duration
		want      freshness
	}{
		{
			name: "nothing cached fetches",
			want: fetchFresh,
		},
		{
			name:      "nothing cached fetches even when immutable",
			immutable: true,
			want:      fetchFresh,
		},
		{
			name:      "immutable is served regardless of age",
			cached:    true,
			immutable: true,
			fetched:   now.Add(-5000 * time.Hour),
			ttl:       ttl,
			want:      serveCached,
		},
		{
			name:      "immutable with zero fetch time is still served",
			cached:    true,
			immutable: true,
			ttl:       ttl,
			want:      serveCached,
		},
		{
			name:    "mutable inside ttl is served",
			cached:  true,
			fetched: now.Add(-1 * time.Minute),
			ttl:     ttl,
			want:    serveCached,
		},
		{
			name:    "mutable exactly at ttl revalidates",
			cached:  true,
			fetched: now.Add(-ttl),
			ttl:     ttl,
			want:    revalidate,
		},
		{
			name:    "mutable past ttl revalidates",
			cached:  true,
			fetched: now.Add(-1 * time.Hour),
			ttl:     ttl,
			want:    revalidate,
		},
		{
			name:    "zero ttl always revalidates",
			cached:  true,
			fetched: now,
			ttl:     0,
			want:    revalidate,
		},
		{
			name:    "negative ttl always revalidates",
			cached:  true,
			fetched: now,
			ttl:     -time.Minute,
			want:    revalidate,
		},
		{
			name:    "clock skew (future fetch time) still serves",
			cached:  true,
			fetched: now.Add(time.Hour),
			ttl:     ttl,
			want:    serveCached,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideFreshness(tt.cached, tt.immutable, tt.fetched, now, tt.ttl)
			if got != tt.want {
				t.Errorf("decideFreshness(cached=%v, immutable=%v, age=%v, ttl=%v) = %v, want %v",
					tt.cached, tt.immutable, now.Sub(tt.fetched), tt.ttl, got, tt.want)
			}
		})
	}
}

func TestIndexPrefix(t *testing.T) {
	tests := []struct{ name, want string }{
		{"a", "1"},
		{"go", "2"},
		{"bar", "3/b"},
		{"serde", "se/rd"},
		{"tokio", "to/ki"},
		{"cc", "2"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := indexPrefix(tt.name); got != tt.want {
				t.Errorf("indexPrefix(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestSafeSegments_RejectsTraversalAndJunk(t *testing.T) {
	bad := []string{
		"",
		"/",
		"//",
		"..",
		"/../etc/passwd",
		"/se/../../etc/passwd",
		"/se/rd/../../../root",
		"/./serde",
		`/se\rd\serde`,
		"/C:/windows",
		"/se//rd",
		"/se/rd/ser\x00de",
		"/se/rd/ser\nde",
		"/se/rd/ser|de",
		"/se/rd/ser*de",
	}
	for _, p := range bad {
		t.Run(strings.ReplaceAll(p, "\x00", "NUL"), func(t *testing.T) {
			if segs, err := safeSegments(p); err == nil {
				t.Errorf("safeSegments(%q) = %v, want error", p, segs)
			}
		})
	}

	good := map[string][]string{
		"/se/rd/serde":  {"se", "rd", "serde"},
		"se/rd/serde":   {"se", "rd", "serde"},
		"/1/a":          {"1", "a"},
		"/3/b/bar":      {"3", "b", "bar"},
		"/config.json":  {"config.json"},
		"/dist/x.tar.z": {"dist", "x.tar.z"},
	}
	for p, want := range good {
		t.Run("ok"+p, func(t *testing.T) {
			got, err := safeSegments(p)
			if err != nil {
				t.Fatalf("safeSegments(%q): %v", p, err)
			}
			if strings.Join(got, "/") != strings.Join(want, "/") {
				t.Errorf("safeSegments(%q) = %v, want %v", p, got, want)
			}
		})
	}
}

// TestCachePaths_StayUnderRoot is the security property: no request path may
// produce a cache location outside the cache root.
func TestCachePaths_StayUnderRoot(t *testing.T) {
	root := filepath.Join("cachedir")

	t.Run("index", func(t *testing.T) {
		got, err := indexCachePath(root, "/se/rd/serde")
		if err != nil {
			t.Fatalf("indexCachePath: %v", err)
		}
		want := filepath.Join(root, "index", "se", "rd", "serde")
		if got != want {
			t.Errorf("indexCachePath = %q, want %q", got, want)
		}
		if _, err := indexCachePath(root, "/../../escape"); err == nil {
			t.Error("indexCachePath accepted a traversal path")
		}
	})

	t.Run("rustup", func(t *testing.T) {
		got, err := rustupCachePath(root, "/dist/2026-08-01/rust-std.tar.xz")
		if err != nil {
			t.Fatalf("rustupCachePath: %v", err)
		}
		want := filepath.Join(root, "rustup", "dist", "2026-08-01", "rust-std.tar.xz")
		if got != want {
			t.Errorf("rustupCachePath = %q, want %q", got, want)
		}
		if _, err := rustupCachePath(root, "/dist/../../escape"); err == nil {
			t.Error("rustupCachePath accepted a traversal path")
		}
	})
}

func TestCrateCachePath(t *testing.T) {
	root := "cachedir"
	tests := []struct {
		name, crate, version, want string
		wantErr                    bool
	}{
		{name: "simple", crate: "serde", version: "1.0.203", want: filepath.Join(root, "crates", "serde", "serde-1.0.203.crate")},
		{name: "underscores and dashes", crate: "proc-macro2", version: "1.0.86", want: filepath.Join(root, "crates", "proc-macro2", "proc-macro2-1.0.86.crate")},
		{name: "case normalised", crate: "Inflector", version: "0.11.4", want: filepath.Join(root, "crates", "inflector", "inflector-0.11.4.crate")},
		{name: "prerelease version", crate: "tokio", version: "1.0.0-beta.1", want: filepath.Join(root, "crates", "tokio", "tokio-1.0.0-beta.1.crate")},
		{name: "build metadata", crate: "tokio", version: "1.0.0+build.5", want: filepath.Join(root, "crates", "tokio", "tokio-1.0.0+build.5.crate")},

		{name: "traversal in name", crate: "../../etc/passwd", version: "1.0.0", wantErr: true},
		{name: "slash in name", crate: "a/b", version: "1.0.0", wantErr: true},
		{name: "dot in name", crate: "..", version: "1.0.0", wantErr: true},
		{name: "empty name", crate: "", version: "1.0.0", wantErr: true},
		{name: "traversal in version", crate: "serde", version: "../../x", wantErr: true},
		{name: "empty version", crate: "serde", version: "", wantErr: true},
		{name: "backslash in version", crate: "serde", version: `1.0\..\x`, wantErr: true},
		{name: "version starting with dot", crate: "serde", version: ".hidden", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := crateCachePath(root, tt.crate, tt.version)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("crateCachePath(%q, %q) = %q, want error", tt.crate, tt.version, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("crateCachePath(%q, %q): %v", tt.crate, tt.version, err)
			}
			if got != tt.want {
				t.Errorf("crateCachePath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsImmutableRustupPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// Dated artifacts are published once and never rewritten.
		{"/dist/2026-08-01/rust-std-nightly-x86_64-unknown-linux-gnu.tar.xz", true},
		{"/dist/2026-08-01/channel-rust-nightly.toml", true},
		{"/dist/2026-08-01/channel-rust-nightly.toml.sha256", true},
		{"dist/2024-12-31/rustc-nightly-src.tar.gz", true},
		{"/dist/2026-08-01", true},

		// Channel manifests are rewritten in place — must revalidate.
		{"/dist/channel-rust-nightly.toml", false},
		{"/dist/channel-rust-stable.toml", false},
		{"/dist/channel-rust-beta.toml.sha256", false},
		{"/rustup/dist/x86_64-unknown-linux-gnu/rustup-init", false},
		{"/rustup/release-stable.toml", false},
		{"", false},
		// Near-misses must not be mistaken for a date segment.
		{"/dist/20260801/x.tar.xz", false},
		{"/dist/v2026-08-01x/x.tar.xz", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isImmutableRustupPath(tt.path); got != tt.want {
				t.Errorf("isImmutableRustupPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsImmutableIndexPath(t *testing.T) {
	// Every index path is mutable — a publish or a yank rewrites the entry.
	for _, p := range []string{"/se/rd/serde", "/config.json", "/1/a"} {
		if isImmutableIndexPath(p) {
			t.Errorf("isImmutableIndexPath(%q) = true, want false", p)
		}
	}
}

func TestExpandDL(t *testing.T) {
	tests := []struct {
		name    string
		tmpl    string
		crate   string
		version string
		want    string
		wantOK  bool
	}{
		{
			name:    "crates.io marker template",
			tmpl:    "https://static.crates.io/crates/{crate}/{crate}-{version}.crate",
			crate:   "serde",
			version: "1.0.203",
			want:    "https://static.crates.io/crates/serde/serde-1.0.203.crate",
			wantOK:  true,
		},
		{
			name:    "marker-free template appends the default suffix",
			tmpl:    "https://static.crates.io/crates",
			crate:   "serde",
			version: "1.0.203",
			want:    "https://static.crates.io/crates/serde/1.0.203/download",
			wantOK:  true,
		},
		{
			name:    "marker-free template with trailing slash",
			tmpl:    "https://example.test/dl/",
			crate:   "tokio",
			version: "1.2.3",
			want:    "https://example.test/dl/tokio/1.2.3/download",
			wantOK:  true,
		},
		{
			name:    "prefix markers",
			tmpl:    "https://example.test/{prefix}/{crate}/{version}.crate",
			crate:   "serde",
			version: "1.0.0",
			want:    "https://example.test/se/rd/serde/1.0.0.crate",
			wantOK:  true,
		},
		{
			name:    "lowerprefix marker lowercases",
			tmpl:    "https://example.test/{lowerprefix}/{crate}",
			crate:   "Inflector",
			version: "0.1.0",
			want:    "https://example.test/in/fl/Inflector",
			wantOK:  true,
		},
		{
			name:    "short name prefix",
			tmpl:    "https://example.test/{prefix}/{crate}-{version}.crate",
			crate:   "cc",
			version: "1.0.0",
			want:    "https://example.test/2/cc-1.0.0.crate",
			wantOK:  true,
		},
		{
			name:    "sha256 marker is unusable",
			tmpl:    "https://example.test/{sha256-checksum}/{crate}",
			crate:   "serde",
			version: "1.0.0",
			wantOK:  false,
		},
		{
			name:   "empty template",
			tmpl:   "",
			crate:  "serde",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := expandDL(tt.tmpl, tt.crate, tt.version)
			if ok != tt.wantOK {
				t.Fatalf("expandDL ok = %v, want %v (got %q)", ok, tt.wantOK, got)
			}
			if ok && got != tt.want {
				t.Errorf("expandDL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseDLTemplate(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "real crates.io config",
			raw:  `{"dl":"https://static.crates.io/crates/{crate}/{crate}-{version}.crate","api":"https://crates.io"}`,
			want: "https://static.crates.io/crates/{crate}/{crate}-{version}.crate",
		},
		{
			name: "marker-free dl",
			raw:  `{"dl":"https://example.test/crates","api":"https://example.test"}`,
			want: "https://example.test/crates",
		},
		{name: "malformed json falls back", raw: `{not json`, want: defaultDLTemplate},
		{name: "missing dl falls back", raw: `{"api":"https://crates.io"}`, want: defaultDLTemplate},
		{name: "blank dl falls back", raw: `{"dl":"   "}`, want: defaultDLTemplate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseDLTemplate([]byte(tt.raw)); got != tt.want {
				t.Errorf("parseDLTemplate(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRewriteConfigJSON(t *testing.T) {
	raw := []byte(`{"dl":"https://static.crates.io/crates/{crate}/{crate}-{version}.crate","api":"https://crates.io","auth-required":false}`)

	out, err := rewriteConfigJSON(raw, "http://10.88.0.1:8083")
	if err != nil {
		t.Fatalf("rewriteConfigJSON: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten config is not valid JSON: %v", err)
	}

	if got["dl"] != "http://10.88.0.1:8083/crates" {
		t.Errorf("dl = %v, want the proxy's /crates route", got["dl"])
	}
	// "api" must survive untouched so `cargo publish` and `cargo search`
	// still reach the real registry.
	if got["api"] != "https://crates.io" {
		t.Errorf("api = %v, want https://crates.io (must not be rewritten)", got["api"])
	}
	// Unknown keys must be preserved rather than dropped.
	if _, ok := got["auth-required"]; !ok {
		t.Error("rewrite dropped the unknown auth-required key")
	}

	if _, err := rewriteConfigJSON([]byte("not json"), "http://x"); err == nil {
		t.Error("rewriteConfigJSON accepted invalid JSON")
	}
}

// TestRewrittenDLRoundTrips pins the contract between the two halves of the
// crate route: whatever we advertise as "dl" must expand to a URL our own
// parseCrateDownloadPath can decode.
func TestRewrittenDLRoundTrips(t *testing.T) {
	base := "http://10.88.0.1:8083"
	out, err := rewriteConfigJSON([]byte(`{"dl":"https://static.crates.io/crates"}`), base)
	if err != nil {
		t.Fatalf("rewriteConfigJSON: %v", err)
	}
	tmpl := parseDLTemplate(out)

	url, ok := expandDL(tmpl, "serde", "1.0.203")
	if !ok {
		t.Fatal("expandDL rejected our own advertised template")
	}
	reqPath := strings.TrimPrefix(url, base)

	name, version, ok := parseCrateDownloadPath(reqPath)
	if !ok {
		t.Fatalf("parseCrateDownloadPath(%q) failed for our own route", reqPath)
	}
	if name != "serde" || version != "1.0.203" {
		t.Errorf("round trip = (%q, %q), want (serde, 1.0.203)", name, version)
	}
}

func TestParseCrateDownloadPath(t *testing.T) {
	tests := []struct {
		path       string
		name, vers string
		ok         bool
	}{
		{path: "/crates/serde/1.0.203/download", name: "serde", vers: "1.0.203", ok: true},
		{path: "/crates/proc-macro2/1.0.86/download", name: "proc-macro2", vers: "1.0.86", ok: true},
		{path: "/crates/tokio/1.0.0-beta.1/download", name: "tokio", vers: "1.0.0-beta.1", ok: true},

		{path: "/crates/serde/1.0.203", ok: false},
		{path: "/crates/serde/1.0.203/fetch", ok: false},
		{path: "/crates/serde/1.0.203/download/extra", ok: false},
		{path: "/crates/serde", ok: false},
		{path: "/crates/", ok: false},
		{path: "/index/se/rd/serde", ok: false},
		{path: "/crates/../../etc/1.0.0/download", ok: false},
		{path: "/crates/se rde/1.0.0/download", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			name, vers, ok := parseCrateDownloadPath(tt.path)
			if ok != tt.ok {
				t.Fatalf("parseCrateDownloadPath(%q) ok = %v, want %v", tt.path, ok, tt.ok)
			}
			if ok && (name != tt.name || vers != tt.vers) {
				t.Errorf("= (%q, %q), want (%q, %q)", name, vers, tt.name, tt.vers)
			}
		})
	}
}

func TestContainerConfigTOML(t *testing.T) {
	got := containerConfigTOML("http://10.88.0.1:8083/")

	// Source replacement is the whole point — without both halves Cargo
	// silently keeps using crates.io.
	for _, want := range []string{
		"[source.crates-io]",
		`replace-with = "ephemerd"`,
		"[source.ephemerd]",
		`registry = "sparse+http://10.88.0.1:8083/index/"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated config missing %q:\n%s", want, got)
		}
	}
	// A doubled slash would produce an index URL Cargo cannot use.
	if strings.Contains(got, "8083//index") {
		t.Errorf("trailing slash in base leaked into the index URL:\n%s", got)
	}
}

func TestContainerConfigDest(t *testing.T) {
	if got := containerConfigDest("linux"); got != "/.cargo" {
		t.Errorf("containerConfigDest(linux) = %q, want /.cargo", got)
	}
	if got := containerConfigDest("darwin"); got != "/.cargo" {
		t.Errorf("containerConfigDest(darwin) = %q, want /.cargo", got)
	}
	if got := containerConfigDest("windows"); got != `C:\.cargo` {
		t.Errorf(`containerConfigDest(windows) = %q, want C:\.cargo`, got)
	}
}

func TestMatchesETag(t *testing.T) {
	tests := []struct {
		ifNoneMatch, tag string
		want             bool
	}{
		{`"abc"`, `"abc"`, true},
		{`W/"abc"`, `"abc"`, true},
		{`"abc"`, `W/"abc"`, true},
		{`"x", "abc"`, `"abc"`, true},
		{`*`, `"abc"`, true},
		{`"abc"`, `"def"`, false},
		{``, `"abc"`, false},
		{`"abc"`, ``, false},
		{``, ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.ifNoneMatch+"|"+tt.tag, func(t *testing.T) {
			if got := matchesETag(tt.ifNoneMatch, tt.tag); got != tt.want {
				t.Errorf("matchesETag(%q, %q) = %v, want %v", tt.ifNoneMatch, tt.tag, got, tt.want)
			}
		})
	}
}
