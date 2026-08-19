package dind

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// gateTestServer returns a Server with only the fields handleContainerCreate
// looks at on the 403 short-circuit path: a non-nil logger and the gate flag.
// The client stays nil so the handler returns 500 from the early nil-check —
// fine for the 403 tests since they should never reach that branch.
func gateTestServer(allow bool) *Server {
	return &Server{
		allowPrivileged: allow,
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func postCreate(t *testing.T, s *Server, body createRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/containers/create", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	s.handleContainerCreate(w, req)
	return w
}

// 403 short-circuit tests: these exercise the full handler path because the
// gate fires before the nil-client check.

func TestHandleContainerCreate_PrivilegedDeniedWhenGateClosed(t *testing.T) {
	s := gateTestServer(false)
	w := postCreate(t, s, createRequest{
		Image:      "alpine:3.20",
		HostConfig: &hostConfig{Privileged: true},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("privileged containers are disabled")) {
		t.Errorf("body did not mention privileged disabled: %s", w.Body.String())
	}
}

func TestHandleContainerCreate_CapAddDeniedWhenGateClosed(t *testing.T) {
	s := gateTestServer(false)
	w := postCreate(t, s, createRequest{
		Image:      "alpine:3.20",
		HostConfig: &hostConfig{CapAdd: []string{"SYS_ADMIN"}},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("SYS_ADMIN")) {
		t.Errorf("body should echo requested cap: %s", w.Body.String())
	}
}

// Pure-function tests for the gate logic. These don't need a Server or a
// containerd client — important because the non-gated paths in
// handleContainerCreate proceed to GetImage which panics on a nil gRPC conn.

func TestCheckPrivilegedGate_AllowedPassesEverything(t *testing.T) {
	cases := []struct {
		name string
		hc   *hostConfig
	}{
		{"nil HostConfig", nil},
		{"empty HostConfig", &hostConfig{}},
		{"Privileged=true", &hostConfig{Privileged: true}},
		{"CapAdd=SYS_ADMIN", &hostConfig{CapAdd: []string{"SYS_ADMIN"}}},
		{"both", &hostConfig{Privileged: true, CapAdd: []string{"NET_ADMIN", "SYS_ADMIN"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, blocked := checkPrivilegedGate(true, tc.hc)
			if blocked {
				t.Errorf("blocked = true with gate open; msg=%q", msg)
			}
			if msg != "" {
				t.Errorf("msg = %q, want empty when not blocked", msg)
			}
		})
	}
}

func TestCheckPrivilegedGate_ClosedRejectsPrivileged(t *testing.T) {
	msg, blocked := checkPrivilegedGate(false, &hostConfig{Privileged: true})
	if !blocked {
		t.Fatal("blocked = false, want true")
	}
	if !strings.Contains(msg, "privileged containers are disabled") {
		t.Errorf("msg = %q, want it to mention 'privileged containers are disabled'", msg)
	}
}

func TestCheckPrivilegedGate_ClosedRejectsCapAdd(t *testing.T) {
	msg, blocked := checkPrivilegedGate(false, &hostConfig{CapAdd: []string{"SYS_ADMIN"}})
	if !blocked {
		t.Fatal("blocked = false, want true")
	}
	if !strings.Contains(msg, "SYS_ADMIN") {
		t.Errorf("msg = %q, want it to echo the requested cap", msg)
	}
}

func TestCheckPrivilegedGate_ClosedAllowsNilHostConfig(t *testing.T) {
	// docker run without -H may omit HostConfig entirely; the gate must
	// not deref nil.
	msg, blocked := checkPrivilegedGate(false, nil)
	if blocked {
		t.Errorf("nil HostConfig blocked: msg=%q", msg)
	}
}

func TestCheckPrivilegedGate_ClosedAllowsZeroHostConfig(t *testing.T) {
	// The common case: Docker CLI always sends HostConfig, mostly with
	// zero fields. The gate must let it through.
	msg, blocked := checkPrivilegedGate(false, &hostConfig{
		Binds:       []string{"/host:/c"},
		NetworkMode: "bridge",
	})
	if blocked {
		t.Errorf("non-elevated HostConfig blocked: msg=%q", msg)
	}
}

// --security-opt gate. Before this was gated, HostConfig.SecurityOpt was
// applied by a loop that ran regardless of allowPrivileged, so
// `docker run --security-opt seccomp=unconfined` stripped the sibling's seccomp
// filter on a host configured with dind.allow_privileged = false.

func TestHandleContainerCreate_SeccompUnconfinedDeniedWhenGateClosed(t *testing.T) {
	s := gateTestServer(false)
	w := postCreate(t, s, createRequest{
		Image:      "alpine:3.20",
		HostConfig: &hostConfig{SecurityOpt: []string{"seccomp=unconfined"}},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("seccomp=unconfined")) {
		t.Errorf("body should echo the refused option: %s", w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("dind.allow_privileged")) {
		t.Errorf("body should name the config knob that governs it: %s", w.Body.String())
	}
}

func TestHandleContainerCreate_ApparmorUnconfinedDeniedWhenGateClosed(t *testing.T) {
	s := gateTestServer(false)
	w := postCreate(t, s, createRequest{
		Image:      "alpine:3.20",
		HostConfig: &hostConfig{SecurityOpt: []string{"apparmor=unconfined"}},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("apparmor=unconfined")) {
		t.Errorf("body should echo the refused option: %s", w.Body.String())
	}
}

func TestCheckPrivilegedGate_ClosedRejectsSecurityOpt(t *testing.T) {
	// The colon spellings are here because the apply loop accepts them too:
	// gate and applier read the same predicate, and this is what stops the
	// two from drifting apart again.
	cases := []struct {
		name string
		opt  string
		want string
	}{
		{"seccomp equals", "seccomp=unconfined", "seccomp=unconfined"},
		{"seccomp colon", "seccomp:unconfined", "seccomp=unconfined"},
		{"apparmor equals", "apparmor=unconfined", "apparmor=unconfined"},
		{"apparmor colon", "apparmor:unconfined", "apparmor=unconfined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, blocked := checkPrivilegedGate(false, &hostConfig{SecurityOpt: []string{tc.opt}})
			if !blocked {
				t.Fatalf("blocked = false for %q, want true", tc.opt)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("msg = %q, want it to name %q", msg, tc.want)
			}
			if !strings.Contains(msg, "dind.allow_privileged") {
				t.Errorf("msg = %q, want it to name the governing config knob", msg)
			}
		})
	}
}

func TestCheckPrivilegedGate_ClosedRejectsSecurityOptAmongOthers(t *testing.T) {
	// The elevating value is not the first element: the gate has to scan the
	// whole slice, not just SecurityOpt[0].
	msg, blocked := checkPrivilegedGate(false, &hostConfig{
		SecurityOpt: []string{"label=disable", "no-new-privileges=true", "seccomp=unconfined"},
	})
	if !blocked {
		t.Fatal("blocked = false, want true")
	}
	if !strings.Contains(msg, "seccomp=unconfined") {
		t.Errorf("msg = %q, want it to name the offending option", msg)
	}
}

func TestCheckPrivilegedGate_ClosedAllowsUnrelatedSecurityOpt(t *testing.T) {
	// Only the values this shim turns into a sandbox-loosening OCI option are
	// elevation. Everything else is ignored by the applier, so refusing it
	// here would be a refusal with no security value — and would break jobs
	// that pass a harmless --security-opt.
	for _, opt := range []string{
		"label=disable",
		"label:user:someuser",
		"no-new-privileges=true",
		"no-new-privileges",
		"seccomp=/path/to/profile.json",
		"apparmor=docker-default",
		"apparmor:docker-default",
		"seccomp=unconfined-ish",
		"unconfined",
		"",
	} {
		t.Run(opt, func(t *testing.T) {
			msg, blocked := checkPrivilegedGate(false, &hostConfig{SecurityOpt: []string{opt}})
			if blocked {
				t.Errorf("--security-opt %q blocked: msg=%q", opt, msg)
			}
		})
	}
}

func TestCheckPrivilegedGate_OpenAllowsSecurityOpt(t *testing.T) {
	// dind.allow_privileged = true has to keep working: it is what lets
	// setup-buildx's container driver run.
	msg, blocked := checkPrivilegedGate(true, &hostConfig{
		SecurityOpt: []string{"seccomp=unconfined", "apparmor=unconfined"},
	})
	if blocked {
		t.Errorf("blocked = true with gate open; msg=%q", msg)
	}
}

func TestHandleContainerCreate_SecurityOptPassesGateWhenOpen(t *testing.T) {
	// With the gate open the request must reach the runtime path. The test
	// server has no containerd client, so "got past the gate" shows up as the
	// 500 from the nil-client check — the point is that it is not a 403.
	setPlatformGOOS(t, "linux") // otherwise the Windows sibling gate answers 501 first
	s := gateTestServer(true)
	for _, opt := range []string{"seccomp=unconfined", "seccomp:unconfined", "apparmor=unconfined", "apparmor:unconfined"} {
		t.Run(opt, func(t *testing.T) {
			w := postCreate(t, s, createRequest{
				Image:      "alpine:3.20",
				HostConfig: &hostConfig{SecurityOpt: []string{opt}},
			})
			if w.Code == http.StatusForbidden {
				t.Fatalf("--security-opt %s refused with the gate open: %s", opt, w.Body.String())
			}
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (nil containerd client); body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleContainerCreate_MalformedRequestStillGets400(t *testing.T) {
	// Request-shape validation runs before the gate, so a broken request gets
	// the accurate 400 rather than a misleading 403 about host policy.
	s := gateTestServer(false)

	t.Run("undecodable body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/containers/create", strings.NewReader("{not json"))
		w := httptest.NewRecorder()
		s.handleContainerCreate(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("missing image", func(t *testing.T) {
		w := postCreate(t, s, createRequest{
			HostConfig: &hostConfig{SecurityOpt: []string{"seccomp=unconfined"}},
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
		}
	})
}

func TestSecurityOptSpecOpts_AppliesElevationWhenAsked(t *testing.T) {
	// The other half of "preserve legitimate behaviour": with the gate open the
	// options must still reach the OCI spec, because that is what setup-buildx's
	// container driver relies on. Both opts are pure spec edits, so they can be
	// applied with a nil client and container.
	for _, opt := range []string{"seccomp=unconfined", "seccomp:unconfined"} {
		t.Run(opt, func(t *testing.T) {
			spec := &oci.Spec{Linux: &ocispec.Linux{Seccomp: &ocispec.LinuxSeccomp{DefaultAction: ocispec.ActErrno}}}
			specOpts := securityOptSpecOpts([]string{opt})
			if len(specOpts) != 1 {
				t.Fatalf("securityOptSpecOpts(%q) produced %d opts, want 1", opt, len(specOpts))
			}
			applySpecOpts(t, spec, specOpts)
			if spec.Linux.Seccomp != nil {
				t.Errorf("seccomp profile still set after %s", opt)
			}
		})
	}
	for _, opt := range []string{"apparmor=unconfined", "apparmor:unconfined"} {
		t.Run(opt, func(t *testing.T) {
			spec := &oci.Spec{Process: &ocispec.Process{ApparmorProfile: "docker-default"}}
			specOpts := securityOptSpecOpts([]string{opt})
			if len(specOpts) != 1 {
				t.Fatalf("securityOptSpecOpts(%q) produced %d opts, want 1", opt, len(specOpts))
			}
			applySpecOpts(t, spec, specOpts)
			if spec.Process.ApparmorProfile != "" {
				t.Errorf("apparmor profile = %q after %s, want cleared", spec.Process.ApparmorProfile, opt)
			}
		})
	}
	t.Run("unrelated opts are no-ops", func(t *testing.T) {
		spec := &oci.Spec{
			Process: &ocispec.Process{ApparmorProfile: "docker-default"},
			Linux:   &ocispec.Linux{Seccomp: &ocispec.LinuxSeccomp{DefaultAction: ocispec.ActErrno}},
		}
		applySpecOpts(t, spec, securityOptSpecOpts([]string{"label=disable", "no-new-privileges=true", "apparmor=docker-default"}))
		if spec.Linux.Seccomp == nil {
			t.Error("seccomp profile cleared by a non-elevating --security-opt")
		}
		if spec.Process.ApparmorProfile != "docker-default" {
			t.Errorf("apparmor profile = %q, want it untouched", spec.Process.ApparmorProfile)
		}
	})
}

func applySpecOpts(t *testing.T, spec *oci.Spec, opts []oci.SpecOpts) {
	t.Helper()
	for _, o := range opts {
		if err := o(context.Background(), nil, nil, spec); err != nil {
			t.Fatalf("applying spec opt: %v", err)
		}
	}
}

func TestElevatingSecurityOpt_NamesTheMechanism(t *testing.T) {
	// securityOptSpecOpts switches on the returned mechanism and the 403
	// message quotes it, so both spellings have to normalise to the same word.
	cases := []struct {
		opt       string
		wantMech  string
		wantEleva bool
	}{
		{"seccomp=unconfined", "seccomp", true},
		{"seccomp:unconfined", "seccomp", true},
		{"apparmor=unconfined", "apparmor", true},
		{"apparmor:unconfined", "apparmor", true},
		{"apparmor=docker-default", "", false},
		{"seccomp=./profile.json", "", false},
		{"label=disable", "", false},
		{"no-new-privileges=true", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.opt, func(t *testing.T) {
			mechanism, elevating := elevatingSecurityOpt(tc.opt)
			if elevating != tc.wantEleva {
				t.Fatalf("elevating = %v, want %v", elevating, tc.wantEleva)
			}
			if mechanism != tc.wantMech {
				t.Errorf("mechanism = %q, want %q", mechanism, tc.wantMech)
			}
		})
	}
}
