package dind

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
