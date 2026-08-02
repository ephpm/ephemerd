package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
)

// dangerousCapabilities are the capabilities that, granted to a CI job running
// as root in a container that shares the host user namespace, hand an attacker
// a documented escape. They must never appear in containerCapabilities.
//
//	CAP_SYS_ADMIN   — mount(2), the single broadest escape primitive
//	CAP_SYS_MODULE  — load a kernel module; game over immediately
//	CAP_SYS_RAWIO   — raw port/memory access
//	CAP_SYS_PTRACE  — attach to processes outside the container's cgroup
//	CAP_NET_ADMIN   — reconfigure the host's networking
//	CAP_DAC_READ_SEARCH — open_by_handle_at, the "shocker" escape
//	CAP_MKNOD       — create a block device for the host disk and read it raw
//	CAP_SYS_BOOT / CAP_SYS_TIME — affect the host, not just the container
var dangerousCapabilities = []string{
	"CAP_SYS_ADMIN",
	"CAP_SYS_MODULE",
	"CAP_SYS_RAWIO",
	"CAP_SYS_PTRACE",
	"CAP_NET_ADMIN",
	"CAP_DAC_READ_SEARCH",
	"CAP_MKNOD",
	"CAP_SYS_BOOT",
	"CAP_SYS_TIME",
}

// requiredCapabilities is the authoritative minimum working set: trimming the
// list past this point breaks apt-get/dpkg/sudo in every job. It is the only
// such list in this package on purpose — two overlapping "required caps"
// assertions would drift apart, which is precisely the failure the pair of
// tests below exists to prevent.
var requiredCapabilities = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FOWNER",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SYS_CHROOT",
	"CAP_NET_BIND_SERVICE",
}

// TestContainerCapabilities_NoDangerousCaps guards the capability allowlist.
// Adding any of these to make one workflow happy would silently remove a
// containment layer for every job on the fleet.
func TestContainerCapabilities_NoDangerousCaps(t *testing.T) {
	for _, bad := range dangerousCapabilities {
		if slices.Contains(containerCapabilities, bad) {
			t.Errorf("containerCapabilities grants %s; see the comment on the var before re-adding it", bad)
		}
	}
}

// TestContainerCapabilities_KeepsRequired is the other half of the guard.
func TestContainerCapabilities_KeepsRequired(t *testing.T) {
	for _, want := range requiredCapabilities {
		if !slices.Contains(containerCapabilities, want) {
			t.Errorf("containerCapabilities is missing %s, which dpkg/sudo/package scripts need", want)
		}
	}
}

// TestCapabilityLists_Disjoint catches an edit that adds a capability to both
// lists, which would make the two tests above contradict each other.
func TestCapabilityLists_Disjoint(t *testing.T) {
	for _, req := range requiredCapabilities {
		if slices.Contains(dangerousCapabilities, req) {
			t.Errorf("%s is listed as both required and dangerous", req)
		}
	}
}

// --- AppArmor confinement decision -----------------------------------------
//
// These exercise the real decision logic (which profile name, if any, a
// container is confined with) rather than the platform shim around it, and
// they run on every platform because the host facts are injected.

// fakeApparmorHost builds an apparmorEnv over an in-memory view of the host.
type fakeApparmorHost struct {
	// files maps path -> contents. A missing key means the read fails.
	files map[string]string
	// dirs is the set of paths that stat succeeds on.
	dirs map[string]bool
	// noParser makes apparmor_parser lookup fail.
	noParser bool
	// loadErr is returned by load; otherwise load writes loadResult into the
	// profiles file, simulating apparmor_parser installing the profile.
	loadErr    error
	loadResult string
	loadCalls  int
}

func (h *fakeApparmorHost) env() apparmorEnv {
	return apparmorEnv{
		readFile: func(name string) ([]byte, error) {
			v, ok := h.files[name]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(v), nil
		},
		stat: func(name string) (fs.FileInfo, error) {
			if h.dirs[name] {
				return nil, nil
			}
			return nil, fs.ErrNotExist
		},
		lookPath: func(string) (string, error) {
			if h.noParser {
				return "", errors.New("not found")
			}
			return "/sbin/apparmor_parser", nil
		},
		load: func(string) error {
			h.loadCalls++
			if h.loadErr != nil {
				return h.loadErr
			}
			h.files[apparmorProfilesPath] = h.loadResult
			return nil
		},
	}
}

// healthyApparmorHost is a host where everything works and the profile is
// already loaded in enforce mode.
func healthyApparmorHost() *fakeApparmorHost {
	return &fakeApparmorHost{
		files: map[string]string{
			apparmorEnabledPath:  "Y\n",
			apparmorProfilesPath: apparmorProfileName + " (enforce)\n",
		},
		dirs:       map[string]bool{apparmorSecurityFSDir: true},
		loadResult: apparmorProfileName + " (enforce)\n",
	}
}

func TestApparmorHostSupport(t *testing.T) {
	tests := []struct {
		name string
		// mutate turns the healthy host into the case under test.
		mutate     func(*fakeApparmorHost)
		wantOK     bool
		wantReason string // substring
	}{
		{
			name:   "healthy host",
			mutate: func(*fakeApparmorHost) {},
			wantOK: true,
		},
		{
			name:       "kernel support disabled",
			mutate:     func(h *fakeApparmorHost) { h.files[apparmorEnabledPath] = "N\n" },
			wantReason: "disabled in the kernel",
		},
		{
			name:       "enabled file empty",
			mutate:     func(h *fakeApparmorHost) { h.files[apparmorEnabledPath] = "" },
			wantReason: "disabled in the kernel",
		},
		{
			name:       "enabled file missing",
			mutate:     func(h *fakeApparmorHost) { delete(h.files, apparmorEnabledPath) },
			wantReason: "cannot read",
		},
		{
			name:   "lowercase y is accepted",
			mutate: func(h *fakeApparmorHost) { h.files[apparmorEnabledPath] = "y" },
			wantOK: true,
		},
		{
			name:       "securityfs not mounted",
			mutate:     func(h *fakeApparmorHost) { h.dirs = nil },
			wantReason: "securityfs is not mounted",
		},
		{
			name:       "parser missing",
			mutate:     func(h *fakeApparmorHost) { h.noParser = true },
			wantReason: "apparmor_parser is not installed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := healthyApparmorHost()
			tt.mutate(h)
			reason := apparmorHostSupport(h.env())
			if tt.wantOK {
				if reason != "" {
					t.Fatalf("apparmorHostSupport() = %q, want supported", reason)
				}
				return
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Fatalf("apparmorHostSupport() = %q, want it to mention %q", reason, tt.wantReason)
			}
		})
	}
}

func TestParseApparmorMode(t *testing.T) {
	tests := []struct {
		name     string
		profiles string
		want     string
	}{
		{
			name:     "enforce",
			profiles: "/usr/sbin/cupsd (enforce)\n" + apparmorProfileName + " (enforce)\n",
			want:     "enforce",
		},
		{
			// The bug this parser exists to fix: a prefix match reports
			// complain mode as "loaded" and leaves jobs unconfined.
			name:     "complain is reported as complain, not enforce",
			profiles: apparmorProfileName + " (complain)\n",
			want:     "complain",
		},
		{
			name:     "profile absent",
			profiles: "docker-default (enforce)\nsnap.foo.bar (enforce)\n",
			want:     "",
		},
		{
			name:     "empty file",
			profiles: "",
			want:     "",
		},
		{
			// A profile whose name merely starts with ours must not match.
			name:     "longer profile with our name as a prefix",
			profiles: apparmorProfileName + "-foo (enforce)\n",
			want:     "",
		},
		{
			name:     "prefix profile does not shadow the real one",
			profiles: apparmorProfileName + "-foo (complain)\n" + apparmorProfileName + " (enforce)\n",
			want:     "enforce",
		},
		{
			name:     "no trailing newline",
			profiles: apparmorProfileName + " (enforce)",
			want:     "enforce",
		},
		{
			name:     "malformed line is skipped",
			profiles: apparmorProfileName + "\n" + apparmorProfileName + " (enforce)\n",
			want:     "enforce",
		},
		{
			name:     "unconfined mode is reported verbatim",
			profiles: apparmorProfileName + " (unconfined)\n",
			want:     "unconfined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseApparmorMode([]byte(tt.profiles), apparmorProfileName); got != tt.want {
				t.Errorf("parseApparmorMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApparmorResolver_Profile(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*fakeApparmorHost)
		wantName   string
		wantReason string // substring; "" means confinement is expected
		wantLoads  int
	}{
		{
			name:     "already loaded in enforce mode",
			mutate:   func(*fakeApparmorHost) {},
			wantName: apparmorProfileName,
		},
		{
			name: "not loaded yet, load succeeds",
			mutate: func(h *fakeApparmorHost) {
				h.files[apparmorProfilesPath] = "docker-default (enforce)\n"
			},
			wantName:  apparmorProfileName,
			wantLoads: 1,
		},
		{
			name: "not loaded, load fails",
			mutate: func(h *fakeApparmorHost) {
				h.files[apparmorProfilesPath] = ""
				h.loadErr = errors.New("apparmor_parser: exit status 1")
			},
			wantReason: "exit status 1",
			wantLoads:  1,
		},
		{
			name: "load reports success but profile is still absent",
			mutate: func(h *fakeApparmorHost) {
				h.files[apparmorProfilesPath] = ""
				h.loadResult = ""
			},
			wantReason: "still not listed",
			wantLoads:  1,
		},
		{
			name: "complain mode is not confinement and is not reloaded",
			mutate: func(h *fakeApparmorHost) {
				h.files[apparmorProfilesPath] = apparmorProfileName + " (complain)\n"
			},
			wantReason: "not enforced",
		},
		{
			name:       "no kernel support",
			mutate:     func(h *fakeApparmorHost) { h.files[apparmorEnabledPath] = "N" },
			wantReason: "disabled in the kernel",
		},
		{
			name:       "securityfs absent",
			mutate:     func(h *fakeApparmorHost) { h.dirs = nil },
			wantReason: "securityfs is not mounted",
		},
		{
			name:       "profiles file unreadable",
			mutate:     func(h *fakeApparmorHost) { delete(h.files, apparmorProfilesPath) },
			wantReason: "cannot read",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := healthyApparmorHost()
			tt.mutate(h)

			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))
			got := newApparmorResolver(h.env()).profile(log)

			if got != tt.wantName {
				t.Errorf("profile() = %q, want %q", got, tt.wantName)
			}
			if h.loadCalls != tt.wantLoads {
				t.Errorf("load called %d times, want %d", h.loadCalls, tt.wantLoads)
			}
			assertApparmorLog(t, buf.Bytes(), tt.wantReason)
		})
	}
}

// assertApparmorLog checks that exactly one line was emitted and that it says
// the right thing. Fail-open is only acceptable if it is loud.
func assertApparmorLog(t *testing.T, out []byte, wantReason string) {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	if len(lines) != 1 || len(lines[0]) == 0 {
		t.Fatalf("want exactly one log line, got %d: %s", len(lines), out)
	}
	var rec struct {
		Level   string `json:"level"`
		Msg     string `json:"msg"`
		Reason  string `json:"reason"`
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal(lines[0], &rec); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, lines[0])
	}
	if wantReason == "" {
		if rec.Level != "INFO" {
			t.Errorf("confined host logged at %s, want INFO: %s", rec.Level, lines[0])
		}
		return
	}
	if rec.Level != "WARN" {
		t.Errorf("unconfined host logged at %s, want WARN: %s", rec.Level, lines[0])
	}
	if !strings.Contains(rec.Reason, wantReason) {
		t.Errorf("log reason = %q, want it to mention %q", rec.Reason, wantReason)
	}
}

// TestApparmorResolver_DoesNotCachePermanently is the regression test for the
// "apparmor reload bricks the node" failure mode: a sync.OnceValue would keep
// handing out the profile name after the kernel dropped the profile, and every
// container create would then fail for the rest of the daemon's lifetime.
func TestApparmorResolver_DoesNotCachePermanently(t *testing.T) {
	h := healthyApparmorHost()
	r := newApparmorResolver(h.env())
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))

	if got := r.profile(log); got != apparmorProfileName {
		t.Fatalf("first profile() = %q, want %q", got, apparmorProfileName)
	}

	// `systemctl reload apparmor` drops the profile — it was never persisted
	// to /etc/apparmor.d. The resolver must notice and reload it.
	h.files[apparmorProfilesPath] = ""
	if got := r.profile(log); got != apparmorProfileName {
		t.Fatalf("after profile was dropped, profile() = %q, want it reloaded", got)
	}
	if h.loadCalls != 1 {
		t.Fatalf("load called %d times, want 1 reload after the profile vanished", h.loadCalls)
	}

	// And if the reload can no longer succeed, it must degrade to unconfined
	// rather than confidently naming a profile the kernel does not have.
	h.files[apparmorProfilesPath] = ""
	h.loadErr = errors.New("parser gone")
	if got := r.profile(log); got != "" {
		t.Fatalf("with a failing reload, profile() = %q, want \"\" (unconfined)", got)
	}
}

// TestApparmorResolver_LogsOncePerStatus keeps the warning from being emitted
// on every container create — jobs are frequent, and a per-job WARN would bury
// everything else in the log.
func TestApparmorResolver_LogsOncePerStatus(t *testing.T) {
	h := healthyApparmorHost()
	h.files[apparmorEnabledPath] = "N"
	r := newApparmorResolver(h.env())

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	for range 5 {
		if got := r.profile(log); got != "" {
			t.Fatalf("profile() = %q, want \"\"", got)
		}
	}
	if n := bytes.Count(bytes.TrimSpace(buf.Bytes()), []byte("\n")) + 1; n != 1 {
		t.Fatalf("got %d log lines across 5 creates, want 1: %s", n, buf.Bytes())
	}

	// A change in status must be reported, though, otherwise an operator who
	// fixes the host never learns that the fix took.
	buf.Reset()
	h.files[apparmorEnabledPath] = "Y"
	if got := r.profile(log); got != apparmorProfileName {
		t.Fatalf("after enabling AppArmor, profile() = %q, want %q", got, apparmorProfileName)
	}
	if buf.Len() == 0 {
		t.Fatal("recovering to a confined state logged nothing")
	}
}

// TestApparmorOpts_PlatformSplit covers the thin platform shim: on non-Linux
// there is deliberately no profile and nothing is logged.
func TestApparmorOpts_PlatformSplit(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	opts := apparmorOpts(log)
	if goruntime.GOOS == "linux" {
		// Both outcomes are valid on a Linux host depending on whether it has
		// AppArmor; the resolver's own tests cover the decision itself.
		for i, o := range opts {
			if o == nil {
				t.Errorf("apparmorOpts()[%d] is nil", i)
			}
		}
		return
	}
	if opts != nil {
		t.Errorf("apparmorOpts() = %v, want nil off Linux", opts)
	}
	if buf.Len() != 0 {
		t.Errorf("apparmorOpts() logged %q off Linux, want silence", buf.String())
	}
}
