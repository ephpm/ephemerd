package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ephpm/ephemerd/mage/download"
	"github.com/ephpm/ephemerd/pkg/runnerversion"
)

// The manifest is the pinned table, nothing more: every platform in
// pinnedSHA256 for RunnerVersion, the same hashes, floor from RunnerFloor.
func TestComposeMatchesPinnedTable(t *testing.T) {
	m, err := Compose("", "", false, "2026-11-10T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != download.RunnerVersion || m.Floor != download.RunnerFloor || m.Channel != runnerversion.ChannelStable {
		t.Fatalf("header: %+v", m)
	}
	arts := download.RunnerArtifacts()
	if len(arts) != 5 || len(m.Platforms) != 5 {
		t.Fatalf("expected the 5 pinned runner platforms, got %d artifacts / %d platforms", len(arts), len(m.Platforms))
	}
	for _, a := range arts {
		p, ok := m.Platforms[a.Platform]
		if !ok || p.SHA256 != a.SHA256 || p.URL != a.URL || !strings.HasSuffix(p.URL, "/"+a.Filename) {
			t.Fatalf("platform %s: %+v vs %+v", a.Platform, p, a)
		}
	}
	for _, want := range []string{"linux-x64", "linux-arm64", "win-x64", "osx-x64", "osx-arm64"} {
		if _, ok := m.Platforms[want]; !ok {
			t.Fatalf("missing platform %s", want)
		}
	}
}

func TestComposeRejectsBadInputs(t *testing.T) {
	if _, err := Compose("99.0.0", "", false, ""); err == nil {
		t.Fatal("floor above version must fail")
	}
	if _, err := Compose("", "nightly", false, ""); err == nil {
		t.Fatal("bad channel must fail")
	}
	if _, err := Compose("", "", false, "yesterday"); err == nil {
		t.Fatal("bad published_at must fail")
	}
}

// Build -> Sign -> Verify end to end with a throwaway key, through the same
// environment knobs CI uses.
func TestBuildSignVerify(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "runner-manifest.json")
	t.Setenv(EnvOut, out)
	t.Setenv(EnvKeygenDir, dir)
	if err := Keygen(); err != nil {
		t.Fatal(err)
	}
	if err := Keygen(); err == nil {
		t.Fatal("second keygen must refuse to overwrite")
	}
	t.Setenv(EnvSecretKeyFile, filepath.Join(dir, "runner-manifest.key"))
	t.Setenv(EnvPublicKeyFile, filepath.Join(dir, "runner-manifest.pub"))

	if err := Build(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var m runnerversion.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != download.RunnerVersion {
		t.Fatalf("built manifest version %q", m.Version)
	}
	if err := Sign(); err != nil {
		t.Fatal(err)
	}
	if err := Verify(); err != nil {
		t.Fatal(err)
	}
	// Tamper: verify must fail closed.
	if err := os.WriteFile(out, []byte(strings.Replace(string(data), m.Floor, "0.0.1", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(); err == nil {
		t.Fatal("tampered manifest must not verify")
	}
	// The secret can also be supplied inline (the CI path).
	key, err := os.ReadFile(filepath.Join(dir, "runner-manifest.key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvSecretKeyFile, "")
	t.Setenv(EnvSecretKey, string(key))
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Sign(); err != nil {
		t.Fatal(err)
	}
	if err := Verify(); err != nil {
		t.Fatal(err)
	}
}
