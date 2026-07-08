//go:build darwin

package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateSandboxProfile(t *testing.T) {
	// Use real directories: the profile resolves symlinks (e.g. /var →
	// /private/var) so rules match the kernel's view of the paths. The
	// expected strings must be the resolved forms.
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	jobDir := filepath.Join(dataDir, "native", "job123")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}

	resolvedData, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	resolvedJob, err := filepath.EvalSymlinks(jobDir)
	if err != nil {
		t.Fatal(err)
	}

	profile := GenerateSandboxProfile(jobDir, dataDir)

	checks := []struct {
		desc   string
		substr string
	}{
		{"allows DNS UDP", `(allow network-outbound (remote udp "localhost:53"))`},
		{"allows DNS TCP", `(allow network-outbound (remote tcp "localhost:53"))`},
		{"blocks localhost", `(deny network-outbound (remote ip "localhost:*"))`},
		{"blocks port binding", `(deny network-bind (local ip "*:*"))`},
		{"blocks sibling job read-data", `(deny file-read-data (subpath "` + resolvedData + `/native"))`},
		{"blocks sibling job writes", `(deny file-write* (subpath "` + resolvedData + `/native"))`},
		{"allows native dir node read (getcwd)", `(allow file-read-data (literal "` + resolvedData + `/native"))`},
		{"blocks SSH dir reads", `(deny file-read* (subpath`},
		{"blocks SSH dir writes", `(deny file-write* (subpath`},
		{"blocks config.toml reads", `(deny file-read* (literal "` + resolvedData + `/config.toml"))`},
		{"blocks config.toml writes", `(deny file-write* (literal "` + resolvedData + `/config.toml"))`},
		{"blocks ephemerd socket reads", `(deny file-read* (literal "` + resolvedData + `/ephemerd.sock"))`},
		{"blocks ephemerd socket writes", `(deny file-write* (literal "` + resolvedData + `/ephemerd.sock"))`},
		{"blocks VM dir reads", `(deny file-read* (subpath "` + resolvedData + `/vm"))`},
		{"blocks VM dir writes", `(deny file-write* (subpath "` + resolvedData + `/vm"))`},
		{"blocks homebrew writes", `(deny file-write* (subpath "/opt/homebrew"))`},
		{"blocks Applications writes", `(deny file-write* (subpath "/Applications"))`},
		{"blocks /usr/local writes", `(deny file-write* (subpath "/usr/local"))`},
		{"re-allows job dir reads", `(allow file-read* (subpath "` + resolvedJob + `"))`},
		{"re-allows job dir read-data", `(allow file-read-data (subpath "` + resolvedJob + `"))`},
		{"allows job dir writes", `(allow file-write* (subpath "` + resolvedJob + `"))`},
		{"allows /private/tmp writes", `(allow file-write* (subpath "/private/tmp"))`},
	}

	for _, c := range checks {
		if !strings.Contains(profile, c.substr) {
			t.Errorf("sandbox profile missing %s: expected substring %q", c.desc, c.substr)
		}
	}
}

// TestGenerateSandboxProfile_ResolvesSymlinks pins the /var → /private/var
// gotcha: a profile written with unresolved paths silently never matches.
func TestGenerateSandboxProfile_ResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	realData := filepath.Join(base, "real-data")
	jobDir := filepath.Join(realData, "native", "j1")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkData := filepath.Join(base, "link-data")
	if err := os.Symlink(realData, linkData); err != nil {
		t.Fatal(err)
	}

	resolvedData, err := filepath.EvalSymlinks(realData)
	if err != nil {
		t.Fatal(err)
	}

	// Generate using the SYMLINK path — the profile must contain the
	// resolved target, and not rules pointing at the symlink.
	profile := GenerateSandboxProfile(filepath.Join(linkData, "native", "j1"), linkData)

	if !strings.Contains(profile, `(deny file-read-data (subpath "`+resolvedData+`/native"))`) {
		t.Errorf("profile should deny the RESOLVED native path %q, got:\n%s", resolvedData, profile)
	}
	if strings.Contains(profile, `(subpath "`+linkData+`/native")`) {
		t.Errorf("profile must not reference the unresolved symlink path %q", linkData)
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

	// Verify expected directories exist. Note: there is no per-job "homebrew"
	// dir — native jobs use the host's shared /opt/homebrew (read-only) so tool
	// checks like `spc doctor` see the host's installed formulae.
	expectedDirs := []string{
		"home",
		"tmp",
		"work",
		"runner",
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
