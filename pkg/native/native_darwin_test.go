//go:build darwin

package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateSandboxProfile(t *testing.T) {
	jobDir := "/tmp/test-native/job123"
	dataDir := "/var/lib/ephemerd"

	profile := GenerateSandboxProfile(jobDir, dataDir)

	// Verify the profile contains key deny rules
	checks := []struct {
		desc    string
		substr  string
	}{
		{"allows DNS UDP", `(allow network-outbound (remote udp "localhost:53"))`},
		{"allows DNS TCP", `(allow network-outbound (remote tcp "localhost:53"))`},
		{"blocks localhost", `(deny network-outbound (remote ip "localhost:*"))`},
		{"blocks port binding", `(deny network-bind (local ip "*:*"))`},
		{"blocks SSH dir", `(deny file-read* (subpath`},
		{"blocks config.toml", `(deny file-read* (literal "/var/lib/ephemerd/config.toml"))`},
		{"blocks ephemerd socket", `(deny file-read* (literal "/var/lib/ephemerd/ephemerd.sock"))`},
		{"blocks VM dir", `(deny file-read* (subpath "/var/lib/ephemerd/vm"))`},
		{"blocks homebrew writes", `(deny file-write* (subpath "/opt/homebrew"))`},
		{"blocks Applications writes", `(deny file-write* (subpath "/Applications"))`},
		{"blocks /usr/local writes", `(deny file-write* (subpath "/usr/local"))`},
		{"allows job dir writes", `(allow file-write* (subpath "/tmp/test-native/job123"))`},
		{"allows /private/tmp writes", `(allow file-write* (subpath "/private/tmp"))`},
	}

	for _, c := range checks {
		if !strings.Contains(profile, c.substr) {
			t.Errorf("sandbox profile missing %s: expected substring %q", c.desc, c.substr)
		}
	}
}

func TestNewCreatesWorkspace(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	runnerSrc := filepath.Join(tmpDir, "runner-src")

	// Create a minimal runner source dir
	if err := os.MkdirAll(runnerSrc, 0o755); err != nil {
		t.Fatal(err)
	}

	r, err := New(dataDir, "test-job-42", "fake-jit-config", runnerSrc, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Verify expected directories exist
	expectedDirs := []string{
		"home",
		"tmp",
		"work",
		"runner",
		filepath.Join("homebrew", "bin"),
		filepath.Join("homebrew", "Cellar"),
		"keychain",
	}
	for _, d := range expectedDirs {
		path := filepath.Join(r.jobDir, d)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("expected directory %s to exist: %v", d, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be a directory", d)
		}
	}
}

func TestCopyRunnerFiles(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join(tmpDir, "src")
	dst := filepath.Join(tmpDir, "dst")

	// Create source tree
	if err := os.MkdirAll(filepath.Join(src, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/bash\necho hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "subdir", "config.json"), []byte(`{"key":"val"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyRunnerFiles(src, dst); err != nil {
		t.Fatalf("copyRunnerFiles() error: %v", err)
	}

	// Verify files were copied
	checks := []struct {
		path    string
		content string
	}{
		{filepath.Join(dst, "run.sh"), "#!/bin/bash\necho hello"},
		{filepath.Join(dst, "subdir", "config.json"), `{"key":"val"}`},
	}
	for _, c := range checks {
		data, err := os.ReadFile(c.path)
		if err != nil {
			t.Errorf("expected file %s: %v", c.path, err)
			continue
		}
		if string(data) != c.content {
			t.Errorf("file %s content = %q, want %q", c.path, string(data), c.content)
		}
	}

	// Verify run.sh is executable
	info, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o100 == 0 {
		t.Error("run.sh should be executable")
	}
}
