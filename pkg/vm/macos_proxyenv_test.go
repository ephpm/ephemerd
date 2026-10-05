package vm

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests EXECUTE the generated script with a real sh and curl, because the
// script is the whole mechanism: a string that merely looks right proves
// nothing about what a shell does with it. The darwin VM code that sends it
// cannot be compiled off macOS, which is why the logic lives here.

func requireShellTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"sh", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH: %v", tool, err)
		}
	}
}

// runStageScript runs the script and returns its status line and the staged
// file's contents ("" if nothing was staged).
func runStageScript(t *testing.T, script, stage string) (status, staged string) {
	t.Helper()
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s\n--- script ---\n%s", err, out, script)
	}
	b, err := os.ReadFile(stage)
	if err == nil {
		staged = string(b)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out)), staged
}

func stagePath(t *testing.T) string {
	return filepath.ToSlash(filepath.Join(t.TempDir(), "runner.env"))
}

// The env the fleet's proxies emit (copied off a live node), pointed at addr.
func proxyEnv(addr string) []string {
	return []string{
		"GOPROXY=http://" + addr + "|direct",
		"GOSUMDB=off",
		"CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse",
		"RUSTUP_DIST_SERVER=http://" + addr + "/rustup",
		"GHREL_PROXY=http://" + addr,
	}
}

func TestMacOSProxyEnvScript_StagesWhenReachable(t *testing.T) {
	requireShellTools(t)
	// 404 on purpose: any HTTP answer proves reachability.
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	stage := stagePath(t)
	env := proxyEnv(addr)

	status, staged := runStageScript(t, macOSProxyEnvScript(env, stage), stage)

	if !macOSProxyEnvStaged(status) || status != "staged 5" {
		t.Fatalf("status = %q, want \"staged 5\"", status)
	}
	if want := strings.Join(env, "\n") + "\n"; staged != want {
		t.Errorf("staged env differs\n got: %q\nwant: %q", staged, want)
	}
}

// One dead address stages NOTHING — not the reachable ones, and not the
// plain-value vars like GOSUMDB=off that only make sense alongside a proxy.
// cargo and rustup do not fall back on an unreachable proxy, so a partial env
// can fail a build that would have succeeded uncached.
func TestMacOSProxyEnvScript_AllOrNothing(t *testing.T) {
	requireShellTools(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := l.Addr().String()
	_ = l.Close()

	env := append(proxyEnv(srv.Listener.Addr().String()), "COMPOSER_REPO_PACKAGIST=http://"+dead)
	stage := stagePath(t)

	status, staged := runStageScript(t, macOSProxyEnvScript(env, stage), stage)

	if macOSProxyEnvStaged(status) {
		t.Fatalf("status = %q: staged although %s is unreachable", status, dead)
	}
	if status != "unreachable "+dead {
		t.Errorf("status = %q, want %q", status, "unreachable "+dead)
	}
	if staged != "" {
		t.Errorf("something was staged despite a dead proxy: %q", staged)
	}
	if _, err := os.Stat(stage + ".new"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %v", err)
	}
}

// Values are written byte-for-byte: the quoted heredoc must not expand $, `,
// or quotes. A GOFLAGS or URL with any of these would otherwise be corrupted
// silently, or execute.
func TestMacOSProxyEnvScript_NoExpansion(t *testing.T) {
	requireShellTools(t)
	stage := stagePath(t)
	env := []string{`WEIRD=$HOME "q" 'sq' ` + "`id`" + ` \n $(echo pwned)`}

	status, staged := runStageScript(t, macOSProxyEnvScript(env, stage), stage)

	if status != "staged 1" {
		t.Fatalf("status = %q, want \"staged 1\"", status)
	}
	if staged != env[0]+"\n" {
		t.Errorf("value was altered by the shell\n got: %q\nwant: %q", staged, env[0]+"\n")
	}
}

func TestMacOSProxyEnvScript_NothingToDo(t *testing.T) {
	for name, env := range map[string][]string{
		"nil":                         nil,
		"empty":                       {},
		"malformed only":              {"NOEQUALS", "=novalue"},
		"newline smuggling":           {"A=b\nrm -rf /"},
		"heredoc terminator smuggled": {heredocDelim},
	} {
		if got := macOSProxyEnvScript(env, "/tmp/x"); got != "" {
			t.Errorf("%s: got a script, want none:\n%s", name, got)
		}
	}
}

// An address that is not plainly host:port is never pasted into the probe
// loop. Since unverified means unadvertised, the whole env is dropped.
func TestMacOSProxyEnvScript_RefusesUnsafeAddress(t *testing.T) {
	env := []string{"GOPROXY=http://evil$(id):8082|direct"}
	if got := macOSProxyEnvScript(env, "/tmp/x"); got != "" {
		t.Errorf("script generated for an unsafe address:\n%s", got)
	}
}

func TestMacOSProxyEnvStaged(t *testing.T) {
	for out, want := range map[string]bool{
		"staged 5\n":                    true,
		"  staged 1":                    true,
		"unreachable 192.168.64.1:8082": false,
		"":                              false,
		"stagedX":                       false,
	} {
		if got := macOSProxyEnvStaged(out); got != want {
			t.Errorf("macOSProxyEnvStaged(%q) = %v, want %v", out, got, want)
		}
	}
}
