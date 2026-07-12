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

	profile := GenerateSandboxProfile(jobDir, dataDir, "", SandboxOptions{})

	checks := []struct {
		desc   string
		substr string
	}{
		{"denies /Users content reads", `(deny file-read-data (subpath "/Users"))`},
		{"denies /Users writes", `(deny file-write* (subpath "/Users"))`},
		{"re-allows /Users node read (getcwd)", `(allow file-read-data (literal "/Users"))`},
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
		{"denies other process inspection (NATIVE-3)", `(deny process-info* (target others))`},
	}

	for _, c := range checks {
		if !strings.Contains(profile, c.substr) {
			t.Errorf("sandbox profile missing %s: expected substring %q", c.desc, c.substr)
		}
	}

	// NATIVE-5 (FIX B): the world-shared /private/tmp write allow must be
	// gone. Jobs write scratch to TMPDIR=<jobDir>/tmp (under the job subtree).
	if strings.Contains(profile, `(allow file-write* (subpath "/private/tmp"))`) {
		t.Errorf("sandbox profile must NOT grant /private/tmp write (NATIVE-5), got:\n%s", profile)
	}

	// Default (non-strict) mode must remain allow-by-default so the parent's
	// already-smoke-tested behavior is unchanged when sandbox_strict=false.
	// Check the preamble LINE, since a comment legitimately says "(allow
	// default)".
	if !containsLine(profile, "(allow default)") {
		t.Errorf("default-mode profile must be allow-by-default, got:\n%s", profile)
	}
	if containsLine(profile, "(deny default)") {
		t.Errorf("default-mode profile must NOT be deny-by-default, got:\n%s", profile)
	}
}

// containsLine reports whether any non-comment line of the profile, after
// trimming whitespace, exactly equals want. Sandbox comments start with ";;",
// so this distinguishes an actual rule from an explanatory comment that
// happens to mention the same S-expression.
func containsLine(profile, want string) bool {
	for _, line := range strings.Split(profile, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ";;") {
			continue
		}
		if trimmed == want {
			return true
		}
	}
	return false
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
	profile := GenerateSandboxProfile(filepath.Join(linkData, "native", "j1"), linkData, "", SandboxOptions{})

	if !strings.Contains(profile, `(deny file-read-data (subpath "`+resolvedData+`/native"))`) {
		t.Errorf("profile should deny the RESOLVED native path %q, got:\n%s", resolvedData, profile)
	}
	if strings.Contains(profile, `(subpath "`+linkData+`/native")`) {
		t.Errorf("profile must not reference the unresolved symlink path %q", linkData)
	}
}

// TestGenerateSandboxProfile_PEMDenies pins NATIVE-1: when the GitHub App
// private_key_path is configured, the profile must deny reading it (and its
// parent dir) explicitly, on top of the broad /Users content deny. The deny
// must not depend on the daemon's HOME.
func TestGenerateSandboxProfile_PEMDenies(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	jobDir := filepath.Join(dataDir, "native", "job123")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A PEM that lives OUTSIDE /Users (e.g. an operator's config dir) — the
	// /Users deny wouldn't cover it, so the explicit deny must.
	pemDir := filepath.Join(base, "secrets")
	if err := os.MkdirAll(pemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pemPath := filepath.Join(pemDir, "app.pem")
	if err := os.WriteFile(pemPath, []byte("-----BEGIN PRIVATE KEY-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedPem, err := filepath.EvalSymlinks(pemPath)
	if err != nil {
		t.Fatal(err)
	}
	resolvedPemDir := filepath.Dir(resolvedPem)

	profile := GenerateSandboxProfile(jobDir, dataDir, pemPath, SandboxOptions{})

	wantLiteral := `(deny file-read* (literal "` + resolvedPem + `"))`
	if !strings.Contains(profile, wantLiteral) {
		t.Errorf("profile missing explicit PEM literal deny %q, got:\n%s", wantLiteral, profile)
	}
	wantDir := `(deny file-read* (subpath "` + resolvedPemDir + `"))`
	if !strings.Contains(profile, wantDir) {
		t.Errorf("profile missing PEM parent-dir deny %q, got:\n%s", wantDir, profile)
	}
}

// TestGenerateSandboxProfile_NoPEM confirms the PEM deny block is omitted
// entirely when no private_key_path is configured (PAT auth), so an empty
// literal deny never lands in the profile.
func TestGenerateSandboxProfile_NoPEM(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	jobDir := filepath.Join(dataDir, "native", "job123")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}

	profile := GenerateSandboxProfile(jobDir, dataDir, "", SandboxOptions{})
	if strings.Contains(profile, `(literal ""))`) {
		t.Errorf("profile must not emit an empty literal deny when no PEM is set, got:\n%s", profile)
	}
	if strings.Contains(profile, "GitHub App private key") {
		t.Errorf("PEM deny block should be absent when no PEM is set, got:\n%s", profile)
	}
}

// TestGenerateSandboxProfile_JobHomeUnaffected confirms the new /Users
// content deny does not touch a job whose HOME lives under the data dir
// (e.g. /var/lib/ephemerd/native/<job>/home): that path is re-allowed for
// read AND write, so the runner's own home is fully usable.
func TestGenerateSandboxProfile_JobHomeUnaffected(t *testing.T) {
	// Use a data dir under a non-/Users root to mirror /var/lib/ephemerd.
	base := t.TempDir() // t.TempDir() on macOS resolves under /private/var
	dataDir := filepath.Join(base, "ephemerd")
	jobDir := filepath.Join(dataDir, "native", "job123")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedJob, err := filepath.EvalSymlinks(jobDir)
	if err != nil {
		t.Fatal(err)
	}

	profile := GenerateSandboxProfile(jobDir, dataDir, "", SandboxOptions{})

	// The job dir (which contains home/) must be re-allowed for read+write,
	// and must not sit under /Users so the /Users deny can't reach it.
	if strings.HasPrefix(resolvedJob, "/Users") {
		t.Fatalf("test setup error: job dir unexpectedly under /Users: %s", resolvedJob)
	}
	for _, want := range []string{
		`(allow file-read* (subpath "` + resolvedJob + `"))`,
		`(allow file-write* (subpath "` + resolvedJob + `"))`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing job-home re-allow %q, got:\n%s", want, profile)
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

	r, err := New(dataDir, "test-job-42", "fake-jit-config", runnerSrc, "", nil)
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

// TestGenerateSandboxProfile_ProcessInfoDeny pins NATIVE-3 (FIX A): the
// profile denies OTHER-process inspection so a sibling native job (same
// _ephemerd uid) can't read the JIT credential from run.sh's argv via
// ps/libproc. See the residual note in the profile: this does NOT close the
// KERN_PROCARGS2 sysctl leak, which sandbox-exec cannot mediate.
func TestGenerateSandboxProfile_ProcessInfoDeny(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	jobDir := filepath.Join(dataDir, "native", "job123")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}

	profile := GenerateSandboxProfile(jobDir, dataDir, "", SandboxOptions{})
	if !strings.Contains(profile, `(deny process-info* (target others))`) {
		t.Errorf("profile must deny other-process inspection (NATIVE-3), got:\n%s", profile)
	}
	// A blanket sysctl-read deny must NOT be present in default mode: it does
	// not close the KERN_PROCARGS2 leak and breaks CPU-count detection. Only
	// strict mode (which allows sysctl-read explicitly) may mention it.
	if strings.Contains(profile, "(deny sysctl-read)") {
		t.Errorf("default profile must not blanket-deny sysctl-read (breaks CPU detection), got:\n%s", profile)
	}
}

// TestGenerateSandboxProfile_StrictMode pins NATIVE-2 (FIX D): strict mode
// emits (deny default) and the core allow-list entries a GHA runner needs,
// while default mode stays (allow default).
func TestGenerateSandboxProfile_StrictMode(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	jobDir := filepath.Join(dataDir, "native", "job123")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedJob, err := filepath.EvalSymlinks(jobDir)
	if err != nil {
		t.Fatal(err)
	}

	strict := GenerateSandboxProfile(jobDir, dataDir, "", SandboxOptions{
		Strict:         true,
		HomebrewPrefix: "/opt/homebrew",
		DeveloperDir:   "/Library/Developer/CommandLineTools",
	})

	// Deny-by-default header. Check the preamble LINE (not a substring
	// anywhere) because an explanatory comment elsewhere legitimately mentions
	// "(allow default)".
	if !containsLine(strict, "(deny default)") {
		t.Errorf("strict profile must be deny-by-default, got:\n%s", strict)
	}
	if containsLine(strict, "(allow default)") {
		t.Errorf("strict profile must NOT emit (allow default) as a rule, got:\n%s", strict)
	}

	// Core allow-list entries the runner + toolchain need.
	wants := []string{
		`(subpath "/usr")`,
		`(subpath "/System")`,
		`(subpath "/opt/homebrew")`,
		`(subpath "/Library/Developer/CommandLineTools")`,
		`(allow file-read* file-write* (subpath "` + resolvedJob + `"))`,
		`(allow process-fork)`,
		`(allow process-exec)`,
		`(allow network-outbound)`,
		`(allow sysctl-read)`,
		`(allow mach-lookup)`,
	}
	for _, w := range wants {
		if !strings.Contains(strict, w) {
			t.Errorf("strict profile missing allow-list entry %q, got:\n%s", w, strict)
		}
	}

	// Tier-1/tier-2 denies must still layer on top in strict mode.
	for _, deny := range []string{
		`(deny process-info* (target others))`,
		`(deny file-write* (subpath "/Users"))`,
		`(deny network-bind (local ip "*:*"))`,
	} {
		if !strings.Contains(strict, deny) {
			t.Errorf("strict profile missing layered deny %q, got:\n%s", deny, strict)
		}
	}
}

// TestGenerateSandboxProfile_StrictNoEmptyDeveloperDir guards against an
// empty DEVELOPER_DIR producing (subpath "") which would match everything and
// silently defeat deny-by-default.
func TestGenerateSandboxProfile_StrictNoEmptyDeveloperDir(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	jobDir := filepath.Join(dataDir, "native", "job123")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	strict := GenerateSandboxProfile(jobDir, dataDir, "", SandboxOptions{Strict: true})
	if strings.Contains(strict, `(subpath ""))`) {
		t.Errorf("strict profile must not emit an empty subpath allow, got:\n%s", strict)
	}
}

// TestBuildLaunchArgs pins NATIVE-6 (FIX C): the runner is launched under a
// shell that applies ulimit -u BEFORE exec'ing sandbox-exec, and the
// profile/jitConfig are passed as positional args (not interpolated into the
// script body) so a jitConfig with shell metacharacters can't break out.
func TestBuildLaunchArgs(t *testing.T) {
	const profile = "/data/native/42/sandbox.sb"
	const jit = "eyJib2d1cyI6InZhbHVlIn0="

	t.Run("with process limit", func(t *testing.T) {
		name, args := buildLaunchArgs(profile, jit, 2048)
		if name != "/bin/sh" {
			t.Fatalf("launch name = %q, want /bin/sh", name)
		}
		if len(args) < 5 || args[0] != "-c" {
			t.Fatalf("args = %v, want -c <script> <label> <profile> <jit>", args)
		}
		script := args[1]
		if !strings.Contains(script, "ulimit -u 2048; ") {
			t.Errorf("script missing ulimit prefix, got: %q", script)
		}
		if !strings.Contains(script, `exec sandbox-exec -f "$1" ./run.sh --jitconfig "$2"`) {
			t.Errorf("script missing sandbox-exec exec line, got: %q", script)
		}
		// profile and jit are passed positionally as $1/$2, NOT interpolated.
		if args[len(args)-2] != profile || args[len(args)-1] != jit {
			t.Errorf("profile/jit not passed positionally: %v", args)
		}
		if strings.Contains(script, jit) || strings.Contains(script, profile) {
			t.Errorf("script body must not interpolate profile/jit (injection risk): %q", script)
		}
	})

	t.Run("no memory or cpu-time limit by default", func(t *testing.T) {
		_, args := buildLaunchArgs(profile, jit, 2048)
		script := args[1]
		if strings.Contains(script, "ulimit -v") || strings.Contains(script, "ulimit -t") {
			t.Errorf("default launch must not set -v/-t (too easy to kill heavy builds): %q", script)
		}
	})

	t.Run("unlimited when maxProc is zero", func(t *testing.T) {
		_, args := buildLaunchArgs(profile, jit, 0)
		script := args[1]
		if strings.Contains(script, "ulimit -u") {
			t.Errorf("maxProc=0 must not set a ulimit, got: %q", script)
		}
		if !strings.Contains(script, "exec sandbox-exec") {
			t.Errorf("script must still exec sandbox-exec, got: %q", script)
		}
	})
}

// TestStrictProfile_ToolchainAllowsResolved guards the two fixes that make
// strict mode actually usable (validated live against clang/curl/git):
//   - /etc must be the RESOLVED /private/etc (curl/openssl reads
//     /private/etc/ssl/openssl.cnf; a bare "/etc" allow matches nothing
//     because the sandbox matches kernel paths).
//   - /private/var/folders (DARWIN_USER_CACHE_DIR) must be read+write or
//     clang/xcrun fail with "couldn't create cache file".
func TestStrictProfile_ToolchainAllowsResolved(t *testing.T) {
	p := GenerateSandboxProfile("/var/lib/ephemerd/native/j1", "/var/lib/ephemerd", "",
		SandboxOptions{Strict: true, HomebrewPrefix: "/opt/homebrew", DeveloperDir: "/Library/Developer/CommandLineTools"})
	if !strings.Contains(p, `(subpath "/private/etc")`) {
		t.Error("strict profile must allow the RESOLVED /private/etc (curl/openssl)")
	}
	if strings.Contains(p, `  (subpath "/etc")`) {
		t.Error("strict profile must NOT use the unresolved /etc (symlink matches nothing)")
	}
	if !strings.Contains(p, `(allow file-read* file-write* (subpath "/private/var/folders"))`) {
		t.Error("strict profile must allow read+write to /private/var/folders (toolchain cache)")
	}
}
