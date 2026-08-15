package cargoproxy

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// freshness is the decision the cache makes for one request. Kept as a pure
// value so the TTL/revalidation policy can be table-tested without a server,
// a clock, or a filesystem.
type freshness int

const (
	// fetchFresh: nothing usable on disk — fetch unconditionally.
	fetchFresh freshness = iota
	// serveCached: the cached copy is within its TTL (or immutable) — serve
	// it and do not touch the network at all.
	serveCached
	// revalidate: a cached copy exists but has aged out — issue a
	// conditional GET (If-None-Match / If-Modified-Since) so the common
	// "nothing changed" case costs one 304 instead of a full body.
	revalidate
)

func (f freshness) String() string {
	switch f {
	case fetchFresh:
		return "fetch"
	case serveCached:
		return "hit"
	case revalidate:
		return "revalidate"
	default:
		return "unknown"
	}
}

// entryMeta is the sidecar record stored next to every cached body. It holds
// the upstream validators so a stale entry can be revalidated cheaply.
type entryMeta struct {
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	ContentType  string    `json:"content_type,omitempty"`
	Fetched      time.Time `json:"fetched"`
}

// decideFreshness is the whole cache policy in one pure function.
//
//   - immutable content (.crate tarballs, dated rustup artifacts) is
//     content-addressed by its URL: once cached it is NEVER refetched and
//     never revalidated, whatever its age.
//   - mutable content (the sparse index, rustup channel manifests) is served
//     from cache inside ttl and conditionally revalidated after that, so we
//     never serve an indefinitely stale index.
//
// A zero or negative ttl means "always revalidate", which is the safe
// direction for mutable data.
func decideFreshness(cached bool, immutable bool, fetched, now time.Time, ttl time.Duration) freshness {
	if !cached {
		return fetchFresh
	}
	if immutable {
		return serveCached
	}
	if ttl > 0 && now.Sub(fetched) < ttl {
		return serveCached
	}
	return revalidate
}

// crateNameRe matches the crate names crates.io permits: ASCII alphanumerics
// plus '-' and '_'. Anything else is rejected rather than sanitized — a name
// we do not recognise must never reach the filesystem.
var crateNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// crateVersionRe is deliberately broader than strict semver (pre-release and
// build metadata are common) but still excludes separators and traversal.
var crateVersionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// safeSegments splits a URL path into segments and rejects anything that
// could escape the cache root or confuse the host filesystem: traversal,
// empty segments, absolute/UNC forms, backslashes, drive letters, control
// characters, and Windows-hostile names. This is the single choke point every
// on-disk cache path goes through.
func safeSegments(urlPath string) ([]string, error) {
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
		for _, r := range s {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("control character in path segment %q", s)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// cachePathUnder joins validated URL-path segments onto a cache root.
func cachePathUnder(root, urlPath string) (string, error) {
	segs, err := safeSegments(urlPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{root}, segs...)...), nil
}

// indexCachePath maps a sparse-index request path (e.g. "/se/rd/serde") to
// its on-disk location under <cacheDir>/index.
func indexCachePath(cacheDir, indexPath string) (string, error) {
	p, err := cachePathUnder(filepath.Join(cacheDir, "index"), indexPath)
	if err != nil {
		return "", fmt.Errorf("index cache path for %q: %w", indexPath, err)
	}
	return p, nil
}

// crateCachePath maps a crate name+version to its on-disk .crate location.
// Both components are validated against the registry's own character rules,
// so a hostile "name" like "../../etc/passwd" is rejected outright rather
// than escaping the cache root.
func crateCachePath(cacheDir, name, version string) (string, error) {
	if !crateNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid crate name %q", name)
	}
	if !crateVersionRe.MatchString(version) {
		return "", fmt.Errorf("invalid crate version %q for %q", version, name)
	}
	lower := strings.ToLower(name)
	return filepath.Join(cacheDir, "crates", lower, lower+"-"+version+".crate"), nil
}

// rustupCachePath maps a rustup dist path to its on-disk location.
func rustupCachePath(cacheDir, distPath string) (string, error) {
	p, err := cachePathUnder(filepath.Join(cacheDir, "rustup"), distPath)
	if err != nil {
		return "", fmt.Errorf("rustup cache path for %q: %w", distPath, err)
	}
	return p, nil
}

// metaPath is the sidecar path for a cached body. Crate names and index
// entries never contain a dot, and rustup artifacts are immutable (no
// sidecar), so ".meta" cannot collide with a real cache entry.
func metaPath(bodyPath string) string { return bodyPath + ".meta" }

// datedRustupRe matches the YYYY-MM-DD segment that rustup uses for pinned,
// immutable nightly/dated artifacts.
var datedRustupRe = regexp.MustCompile(`(^|/)\d{4}-\d{2}-\d{2}(/|$)`)

// isImmutableRustupPath reports whether a rustup dist path is content-stable.
//
// Dated artifacts (dist/2026-08-01/rust-std-nightly-x86_64...tar.xz) are
// published once and never rewritten, so they cache forever. The channel
// manifests (dist/channel-rust-nightly.toml and its .sha256/.asc siblings)
// are rewritten daily in place, so they must revalidate — caching those
// forever would pin every job to the toolchain that happened to be current
// when the daemon first started.
func isImmutableRustupPath(distPath string) bool {
	return datedRustupRe.MatchString(distPath)
}

// isImmutableIndexPath reports whether a sparse-index path is immutable.
// Nothing in the index is: entries gain a line whenever a version is
// published or yanked, and config.json can be re-pointed. Kept as a named
// function so the call sites read symmetrically and the intent is explicit
// rather than an unexplained "false".
func isImmutableIndexPath(string) bool { return false }

// indexPrefix returns the sparse-index directory prefix for a crate name,
// per the registry index layout: 1-char names live under "1", 2-char under
// "2", 3-char under "3/<first char>", and everything else under the first
// two characters then the next two.
func indexPrefix(name string) string {
	switch len(name) {
	case 0:
		return ""
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return path.Join("3", name[0:1])
	default:
		return path.Join(name[0:2], name[2:4])
	}
}

// defaultDLTemplate is the download URL used when the upstream registry's
// config.json is not available (first crate request after a restart with an
// empty index cache). It is crates.io's published template.
const defaultDLTemplate = "https://static.crates.io/crates/{crate}/{crate}-{version}.crate"

// registryConfig is the subset of a sparse registry's config.json we care
// about. Everything else is preserved verbatim on rewrite via rawConfig.
type registryConfig struct {
	DL  string `json:"dl"`
	API string `json:"api,omitempty"`
}

// parseDLTemplate extracts the "dl" template from a registry config.json.
// A config.json we cannot parse falls back to the crates.io default rather
// than failing the request — the worst case is that we proxy from the
// canonical CDN instead of a mirror.
func parseDLTemplate(raw []byte) string {
	var cfg registryConfig
	if err := json.Unmarshal(raw, &cfg); err != nil || strings.TrimSpace(cfg.DL) == "" {
		return defaultDLTemplate
	}
	return cfg.DL
}

// dlMarkers are the substitutions a registry may use in its "dl" template.
var dlMarkers = []string{"{crate}", "{version}", "{prefix}", "{lowerprefix}", "{sha256-checksum}"}

// expandDL turns a registry "dl" template into a concrete upstream URL for
// one crate version, implementing the registry-index rules:
//
//   - {crate}, {version}, {prefix}, {lowerprefix} are substituted;
//   - a template containing NO markers gets "/{crate}/{version}/download"
//     appended, which is the documented default;
//   - {sha256-checksum} cannot be expanded here (the checksum lives in the
//     index entry, which we do not parse), so such a template is reported
//     unusable and the caller falls back to the crates.io default.
func expandDL(tmpl, name, version string) (string, bool) {
	tmpl = strings.TrimSpace(tmpl)
	if tmpl == "" {
		return "", false
	}
	if strings.Contains(tmpl, "{sha256-checksum}") {
		return "", false
	}

	hasMarker := false
	for _, m := range dlMarkers {
		if strings.Contains(tmpl, m) {
			hasMarker = true
			break
		}
	}
	if !hasMarker {
		return strings.TrimRight(tmpl, "/") + "/" + name + "/" + version + "/download", true
	}

	lower := strings.ToLower(name)
	r := strings.NewReplacer(
		"{crate}", name,
		"{version}", version,
		"{prefix}", indexPrefix(name),
		"{lowerprefix}", indexPrefix(lower),
	)
	return r.Replace(tmpl), true
}

// rewriteConfigJSON rewrites a registry config.json so Cargo fetches crate
// tarballs from THIS proxy instead of the upstream CDN, while leaving every
// other field — notably "api", which `cargo publish`/`cargo search` use —
// pointing at the real registry. The rewritten "dl" is deliberately
// marker-free, so Cargo appends "/{crate}/{version}/download" and we get a
// single, unambiguous request shape to route.
//
// Unknown fields are preserved: the config is decoded into a generic map so
// a future registry key is passed through rather than silently dropped.
func rewriteConfigJSON(raw []byte, base string) ([]byte, error) {
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("parsing registry config.json: %w", err)
	}
	generic["dl"] = strings.TrimRight(base, "/") + cratesRoute
	out, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("re-encoding registry config.json: %w", err)
	}
	return out, nil
}

// parseCrateDownloadPath splits the crate-download route Cargo generates
// from our rewritten "dl" — "/crates/<name>/<version>/download" — into its
// components. Any other shape is rejected.
func parseCrateDownloadPath(urlPath string) (name, version string, ok bool) {
	rest := strings.TrimPrefix(urlPath, cratesRoute+"/")
	if rest == urlPath {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[2] != "download" {
		return "", "", false
	}
	if !crateNameRe.MatchString(parts[0]) || !crateVersionRe.MatchString(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// containerConfigTOML renders the Cargo config that gets mounted into every
// job container.
//
// Source replacement is the ONLY mechanism Cargo offers for redirecting
// crates.io, and it is read exclusively from config files — CARGO_SOURCE_*
// environment variables are silently ignored (verified against cargo stable:
// with the env vars set, cargo still reported "Updating crates.io index").
// Hence a generated file rather than an env var.
func containerConfigTOML(base string) string {
	base = strings.TrimRight(base, "/")
	return `# Generated by ephemerd. Do NOT edit — regenerated on every daemon start.
#
# Routes crates.io through ephemerd's on-host pull-through cache so repeated
# CI builds do not re-download the same .crate tarballs. Delete the
# [cargo_proxy] block from ephemerd's config.toml to turn this off.
#
# A repository's own .cargo/config.toml still wins: Cargo prefers config
# closer to the workspace, so a project that pins its own mirror is
# unaffected by this file.

[source.crates-io]
replace-with = "ephemerd"

[source.ephemerd]
registry = "sparse+` + base + indexRoute + `/"

[net]
retry = 3
`
}

// inertConfigTOML renders the config the proxy installs when it has withdrawn
// itself (see the package comment, layer 3).
//
// The point is what is ABSENT: no [source] table at all, so Cargo falls back
// to its built-in crates-io source and talks to the registry directly. It has
// to be a whole valid config rather than a deleted file, because the mount
// destination is a directory Cargo searches — leaving a stale caching config
// behind, or a partial one, is what would break builds.
//
// [net] retry is kept: it is useful on its own and costs nothing.
func inertConfigTOML() string {
	return `# Generated by ephemerd. Do NOT edit — regenerated on every daemon start.
#
# ephemerd's Cargo pull-through cache is NOT in use right now: the daemon's
# proxy stopped answering its own health probe, so it withdrew itself rather
# than take Rust builds down with it. Builds go straight to crates.io —
# slower on a cold cache, but green. The caching config is restored
# automatically as soon as the proxy answers again.
#
# There is deliberately no [source] replacement below. Cargo has no fallback
# for a dead replacement source (unlike Go's GOPROXY="…|direct"), so when the
# cache cannot be trusted the only safe configuration is none at all.

[net]
retry = 3
`
}

// containerConfigDest is the container path the generated .cargo directory is
// mounted at. Cargo walks the current directory and EVERY ancestor looking
// for .cargo/config.toml, so placing it at the filesystem root makes it apply
// to any workspace path a job might use — no knowledge of the checkout
// location, the job user's home, or the image's CARGO_HOME required, and
// CARGO_HOME itself is left alone so it stays writable.
func containerConfigDest(goos string) string {
	if goos == "windows" {
		return `C:\.cargo`
	}
	return "/.cargo"
}
