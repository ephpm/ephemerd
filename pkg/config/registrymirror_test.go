package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// loadRegistryMirrorTOML writes a minimal valid config with the given
// [registry_mirror] block and runs it through Load, returning the validation
// error (if any). Hostnames here are RFC 2606 / RFC 5737 documentation names
// and addresses — no real site's cache is encoded in this file.
func loadRegistryMirrorTOML(t *testing.T, block string) (*Config, error) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "ghp_test123")

	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[github]\nowner = \"testorg\"\n\n[registry_mirror]\n" + block
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// TestRegistryMirror_LoadFullBlock pins the config surface: every key parses
// off a realistic block, including the per-registry sub-table.
func TestRegistryMirror_LoadFullBlock(t *testing.T) {
	cfg, err := loadRegistryMirrorTOML(t, `enabled = true
endpoint = "http://cache.example.com:5000"
registries = ["docker.io", "ghcr.io"]
fallback_to_origin = false
forward_credentials = true

[registry_mirror.mirrors]
"quay.io" = "https://quay-cache.example.com"
`)
	if err != nil {
		t.Fatalf("Load rejected a valid registry_mirror block: %v", err)
	}
	rm := cfg.RegistryMirror
	if !rm.Enabled {
		t.Error("Enabled = false, want true")
	}
	if rm.Endpoint != "http://cache.example.com:5000" {
		t.Errorf("Endpoint = %q", rm.Endpoint)
	}
	if !reflect.DeepEqual(rm.Registries, []string{"docker.io", "ghcr.io"}) {
		t.Errorf("Registries = %v", rm.Registries)
	}
	if rm.ResolvedFallbackToOrigin() {
		t.Error("ResolvedFallbackToOrigin() = true, want false (explicitly disabled)")
	}
	if !rm.ForwardCredentials {
		t.Error("ForwardCredentials = false, want true")
	}
	if got := rm.Mirrors["quay.io"]; got != "https://quay-cache.example.com" {
		t.Errorf("Mirrors[quay.io] = %q", got)
	}
}

// TestRegistryMirror_AbsentBlockIsInert is the no-regression guarantee: a
// config with no [registry_mirror] at all must resolve to "no mirror", which
// is what keeps every pull path byte-identical to its pre-feature behavior.
func TestRegistryMirror_AbsentBlockIsInert(t *testing.T) {
	cfg, err := loadRegistryMirrorTOML(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RegistryMirror.Enabled {
		t.Error("Enabled defaulted to true")
	}
	if got := cfg.RegistryMirror.ResolvedMirrors(); got != nil {
		t.Errorf("ResolvedMirrors() = %v, want nil for an absent block", got)
	}
}

// TestRegistryMirror_FallbackDefaultsOpen is the safety default the whole
// feature hangs on: an operator who configures a mirror and says nothing about
// fallback must keep reaching the origin registry, so a dead cache degrades
// the node to WAN speed instead of failing every job on it.
func TestRegistryMirror_FallbackDefaultsOpen(t *testing.T) {
	cfg, err := loadRegistryMirrorTOML(t, "enabled = true\nendpoint = \"http://cache.example.com:5000\"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RegistryMirror.ResolvedFallbackToOrigin() {
		t.Error("ResolvedFallbackToOrigin() = false with the key unset; the default must be fail-open")
	}
	if cfg.RegistryMirror.ForwardCredentials {
		t.Error("ForwardCredentials defaulted to true; credentials must not reach a mirror unless asked for")
	}
}

// TestRegistryMirror_ResolvedMirrors covers the flattening of
// endpoint+registries and the per-registry override table into the single
// lookup the pull paths use, including host normalization.
func TestRegistryMirror_ResolvedMirrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  RegistryMirrorConfig
		want map[string]string
	}{
		{
			name: "disabled yields nothing even when configured",
			cfg: RegistryMirrorConfig{
				Endpoint: "http://cache.example.com:5000",
			},
			want: nil,
		},
		{
			name: "endpoint alone defaults to mirroring docker.io",
			cfg: RegistryMirrorConfig{
				Enabled:  true,
				Endpoint: "http://cache.example.com:5000",
			},
			want: map[string]string{"docker.io": "http://cache.example.com:5000"},
		},
		{
			name: "one endpoint serves every listed registry",
			cfg: RegistryMirrorConfig{
				Enabled:    true,
				Endpoint:   "https://cache.example.com",
				Registries: []string{"docker.io", "ghcr.io"},
			},
			want: map[string]string{
				"docker.io": "https://cache.example.com",
				"ghcr.io":   "https://cache.example.com",
			},
		},
		{
			name: "trailing slashes are trimmed so /v2 joins cleanly",
			cfg: RegistryMirrorConfig{
				Enabled:  true,
				Endpoint: "http://cache.example.com:5000/",
			},
			want: map[string]string{"docker.io": "http://cache.example.com:5000"},
		},
		{
			name: "docker hub's other spellings fold to docker.io",
			cfg: RegistryMirrorConfig{
				Enabled:    true,
				Endpoint:   "http://cache.example.com:5000",
				Registries: []string{"index.docker.io", "registry-1.docker.io", "DOCKER.IO"},
			},
			want: map[string]string{"docker.io": "http://cache.example.com:5000"},
		},
		{
			name: "per-registry entry adds its own cache",
			cfg: RegistryMirrorConfig{
				Enabled:  true,
				Endpoint: "http://hub-cache.example.com:5000",
				Mirrors:  map[string]string{"ghcr.io": "http://ghcr-cache.example.com:5000"},
			},
			want: map[string]string{
				"docker.io": "http://hub-cache.example.com:5000",
				"ghcr.io":   "http://ghcr-cache.example.com:5000",
			},
		},
		{
			name: "per-registry entry wins over endpoint for the same host",
			cfg: RegistryMirrorConfig{
				Enabled:    true,
				Endpoint:   "http://general.example.com:5000",
				Registries: []string{"docker.io"},
				Mirrors:    map[string]string{"docker.io": "http://specific.example.com:5000"},
			},
			want: map[string]string{"docker.io": "http://specific.example.com:5000"},
		},
		{
			name: "mirrors alone, no endpoint",
			cfg: RegistryMirrorConfig{
				Enabled: true,
				Mirrors: map[string]string{"ghcr.io": "http://ghcr-cache.example.com:5000"},
			},
			want: map[string]string{"ghcr.io": "http://ghcr-cache.example.com:5000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.ResolvedMirrors()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ResolvedMirrors() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRegistryMirror_ValidationRejects is the fail-fast surface. A typo'd
// endpoint has to stop the daemon at load with a message naming the exact key,
// rather than surfacing hours later as an unexplained pull failure on the
// first job of the day.
func TestRegistryMirror_ValidationRejects(t *testing.T) {
	tests := []struct {
		name      string
		block     string
		wantParts []string
	}{
		{
			name:      "missing scheme is the common typo",
			block:     "enabled = true\nendpoint = \"cache.example.com:5000\"\n",
			wantParts: []string{"registry_mirror.endpoint", "scheme", "http://cache.example.com:5000"},
		},
		{
			name:      "non-http scheme",
			block:     "enabled = true\nendpoint = \"ftp://cache.example.com\"\n",
			wantParts: []string{"registry_mirror.endpoint", "ftp", "http:// or https://"},
		},
		{
			name:      "scheme but no host",
			block:     "enabled = true\nendpoint = \"http://\"\n",
			wantParts: []string{"registry_mirror.endpoint", "no host"},
		},
		{
			name:      "unparseable URL",
			block:     "enabled = true\nendpoint = \"http://cache.example.com:port\"\n",
			wantParts: []string{"registry_mirror.endpoint", "not a valid URL"},
		},
		{
			name:      "enabled with nothing to route",
			block:     "enabled = true\n",
			wantParts: []string{"registry_mirror.endpoint", "required", "registry_mirror.mirrors"},
		},
		{
			name: "bad URL in the per-registry table names that entry",
			block: "enabled = true\n\n[registry_mirror.mirrors]\n" +
				"\"ghcr.io\" = \"ghcr-cache.example.com:5000\"\n",
			wantParts: []string{`registry_mirror.mirrors["ghcr.io"]`, "scheme"},
		},
		{
			name: "empty URL in the per-registry table",
			block: "enabled = true\n\n[registry_mirror.mirrors]\n" +
				"\"ghcr.io\" = \"\"\n",
			wantParts: []string{`registry_mirror.mirrors["ghcr.io"]`, "is empty"},
		},
		{
			// Validation runs on the values regardless of the toggle, so a
			// mirror staged ahead of a rollout is known-good before anyone
			// flips it on.
			name:      "malformed endpoint is rejected even while disabled",
			block:     "enabled = false\nendpoint = \"cache.example.com:5000\"\n",
			wantParts: []string{"registry_mirror.endpoint", "scheme"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadRegistryMirrorTOML(t, tt.block)
			if err == nil {
				t.Fatalf("Load accepted %q; want a startup failure", tt.block)
			}
			msg := err.Error()
			for _, want := range tt.wantParts {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
		})
	}
}

// TestRegistryMirror_ValidationAccepts guards against over-eager validation
// rejecting shapes an operator legitimately writes.
func TestRegistryMirror_ValidationAccepts(t *testing.T) {
	blocks := []string{
		"enabled = true\nendpoint = \"http://cache.example.com:5000\"\n",
		"enabled = true\nendpoint = \"https://cache.example.com\"\n",
		// A path prefix, as Harbor/Zot proxy projects produce.
		"enabled = true\nendpoint = \"https://harbor.example.com/v2/dockerhub-proxy\"\n",
		// Disabled and empty: the shipped default.
		"",
		// Staged but not turned on.
		"enabled = false\nendpoint = \"http://cache.example.com:5000\"\n",
		// Per-registry only.
		"enabled = true\n\n[registry_mirror.mirrors]\n\"ghcr.io\" = \"http://ghcr-cache.example.com:5000\"\n",
	}
	for _, block := range blocks {
		if _, err := loadRegistryMirrorTOML(t, block); err != nil {
			t.Errorf("Load rejected %q: %v", block, err)
		}
	}
}

// TestNormalizeRegistryHost pins the folding the mirror lookup depends on:
// the pull paths look a mirror up by the host containerd hands them, which is
// always the "docker.io" spelling for Docker Hub.
func TestNormalizeRegistryHost(t *testing.T) {
	tests := []struct{ in, want string }{
		{"docker.io", "docker.io"},
		{"DOCKER.IO", "docker.io"},
		{"  docker.io  ", "docker.io"},
		{"index.docker.io", "docker.io"},
		{"registry-1.docker.io", "docker.io"},
		{"https://docker.io", "docker.io"},
		{"https://index.docker.io/v1/", "docker.io"},
		{"ghcr.io", "ghcr.io"},
		{"registry.example.com:5000", "registry.example.com:5000"},
		{"", ""},
		{"   ", ""},
	}
	for _, tt := range tests {
		if got := NormalizeRegistryHost(tt.in); got != tt.want {
			t.Errorf("NormalizeRegistryHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
