package native

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNew_CreatesWorkspaceDirs(t *testing.T) {
	tmp := t.TempDir()
	cfg := Config{
		DataDir:   tmp,
		JobID:     "test-job-1",
		JITConfig: "fake-jit",
		RunnerDir: filepath.Join(tmp, "runner-template"),
		Log:       testLogger(),
	}

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	expectedDirs := []string{
		filepath.Join(tmp, "native-jobs", "test-job-1", "home"),
		filepath.Join(tmp, "native-jobs", "test-job-1", "tmp"),
		filepath.Join(tmp, "native-jobs", "test-job-1", "work"),
		filepath.Join(tmp, "native-jobs", "test-job-1", "runner"),
		filepath.Join(tmp, "native-jobs", "test-job-1", "homebrew", "bin"),
		filepath.Join(tmp, "native-jobs", "test-job-1", "keychain"),
	}

	for _, dir := range expectedDirs {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("expected dir %s to exist: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be a directory", dir)
		}
	}

	if r.workspace != filepath.Join(tmp, "native-jobs", "test-job-1") {
		t.Errorf("workspace = %q, want %q", r.workspace, filepath.Join(tmp, "native-jobs", "test-job-1"))
	}
}

func TestNew_FailsOnBadDataDir(t *testing.T) {
	cfg := Config{
		DataDir:   "/dev/null/impossible",
		JobID:     "fail-job",
		JITConfig: "fake",
		RunnerDir: "/nonexistent",
		Log:       testLogger(),
	}

	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error for invalid data dir")
	}
}

func TestGenerateSandboxProfile_ContainsRFC1918Denies(t *testing.T) {
	profile := GenerateSandboxProfile("/tmp/workspace", "/var/lib/ephemerd")

	requiredPatterns := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"deny network-bind",
		"deny file-write* (subpath \"/opt/homebrew\")",
		"deny file-write* (subpath \"/Applications\")",
		"deny file-write* (subpath \"/usr/local\")",
		"deny file-read* (literal \"/var/lib/ephemerd/config.toml\")",
		"deny file-read* (literal \"/var/lib/ephemerd/ephemerd.sock\")",
		"allow file-read* (subpath \"/tmp/workspace\")",
		"allow file-write* (subpath \"/tmp/workspace\")",
		"(version 1)",
		"(allow default)",
	}

	for _, p := range requiredPatterns {
		if !strings.Contains(profile, p) {
			t.Errorf("sandbox profile missing pattern: %q", p)
		}
	}
}

func TestGenerateSandboxProfile_ContainsSSHDeny(t *testing.T) {
	profile := GenerateSandboxProfile("/tmp/workspace", "/var/lib/ephemerd")

	// Should deny access to the user's .ssh directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	expected := "deny file-read* (subpath \"" + homeDir + "/.ssh\")"
	if !strings.Contains(profile, expected) {
		t.Errorf("sandbox profile missing SSH deny: %q", expected)
	}
}

func TestGenerateSandboxProfile_WorkspaceAllowed(t *testing.T) {
	workspace := "/custom/job/workspace"
	profile := GenerateSandboxProfile(workspace, "/data")

	if !strings.Contains(profile, "(allow file-read* (subpath \""+workspace+"\"))") {
		t.Error("workspace read not allowed")
	}
	if !strings.Contains(profile, "(allow file-write* (subpath \""+workspace+"\"))") {
		t.Error("workspace write not allowed")
	}
}

func TestAddress_ReturnsEmpty(t *testing.T) {
	r := &Runner{}
	if addr := r.Address(); addr != "" {
		t.Errorf("Address() = %q, want empty", addr)
	}
}

func TestBuildEnv_ContainsExpectedVars(t *testing.T) {
	tmp := t.TempDir()
	r := &Runner{
		cfg: Config{
			DataDir: tmp,
			JobID:   "env-test",
			Log:     testLogger(),
		},
		workspace: filepath.Join(tmp, "native-jobs", "env-test"),
	}

	env := r.buildEnv("/tmp/keychain.db")

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	expected := map[string]string{
		"HOME":            filepath.Join(r.workspace, "home"),
		"TMPDIR":          filepath.Join(r.workspace, "tmp"),
		"RUNNER_TEMP":     filepath.Join(r.workspace, "tmp"),
		"RUNNER_WORK":     filepath.Join(r.workspace, "work"),
		"HOMEBREW_PREFIX": filepath.Join(r.workspace, "homebrew"),
		"LANG":            "en_US.UTF-8",
	}

	for k, want := range expected {
		got, ok := envMap[k]
		if !ok {
			t.Errorf("env var %s not found", k)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// PATH should contain the job's homebrew bin
	if !strings.Contains(envMap["PATH"], filepath.Join(r.workspace, "homebrew", "bin")) {
		t.Errorf("PATH does not contain homebrew bin: %s", envMap["PATH"])
	}

	// Keychain path should be set
	if envMap["EPHEMERD_KEYCHAIN"] != "/tmp/keychain.db" {
		t.Errorf("EPHEMERD_KEYCHAIN = %q, want /tmp/keychain.db", envMap["EPHEMERD_KEYCHAIN"])
	}
}

func TestBuildEnv_EmptyKeychain(t *testing.T) {
	r := &Runner{
		cfg: Config{
			DataDir: "/tmp",
			JobID:   "no-keychain",
			Log:     testLogger(),
		},
		workspace: "/tmp/native-jobs/no-keychain",
	}

	env := r.buildEnv("")

	for _, e := range env {
		if strings.HasPrefix(e, "EPHEMERD_KEYCHAIN=") {
			t.Error("EPHEMERD_KEYCHAIN should not be set when keychain path is empty")
		}
	}
}

func TestSetupHomebrewOverlay_NoSystemBrew(t *testing.T) {
	// If /opt/homebrew/bin doesn't exist, setupHomebrewOverlay should return
	// an error but not panic.
	tmp := t.TempDir()
	r := &Runner{
		cfg: Config{
			DataDir: tmp,
			JobID:   "brew-test",
			Log:     testLogger(),
		},
		workspace: filepath.Join(tmp, "native-jobs", "brew-test"),
	}

	if err := os.MkdirAll(filepath.Join(r.workspace, "homebrew", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	// On systems without /opt/homebrew/bin, this returns an error
	err := r.setupHomebrewOverlay()
	if err != nil {
		// Expected on systems without Homebrew — verify it's a ReadDir error
		if !strings.Contains(err.Error(), "/opt/homebrew/bin") {
			t.Errorf("unexpected error: %v", err)
		}
	}
	// If it succeeds (system has Homebrew), that's fine too
}
