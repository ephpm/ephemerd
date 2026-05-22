package dind

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/containerd/containerd/v2/client"
)

// gateTestServer returns a Server in the minimum state required to exercise
// handleContainerCreate's pre-pull validation path. The handler bails out
// before touching the client when the image is invalid or the privileged
// gate rejects the request, so a zero-value *client.Client is enough — we
// just need it non-nil to pass the early nil-check.
func gateTestServer(allow bool) *Server {
	return &Server{
		client:          &client.Client{},
		allowPrivileged: allow,
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		containers:      map[string]*containerEntry{},
		images:          map[string]*imageEntry{},
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

func TestHandleContainerCreate_PrivilegedDeniedWhenGateClosed(t *testing.T) {
	s := gateTestServer(false)
	body := createRequest{
		Image:      "alpine:3.20",
		HostConfig: &hostConfig{Privileged: true},
	}
	w := postCreate(t, s, body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("privileged containers are disabled")) {
		t.Errorf("body did not mention privileged disabled: %s", w.Body.String())
	}
}

func TestHandleContainerCreate_CapAddDeniedWhenGateClosed(t *testing.T) {
	s := gateTestServer(false)
	body := createRequest{
		Image:      "alpine:3.20",
		HostConfig: &hostConfig{CapAdd: []string{"SYS_ADMIN"}},
	}
	w := postCreate(t, s, body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("SYS_ADMIN")) {
		t.Errorf("body should echo requested cap: %s", w.Body.String())
	}
}

func TestHandleContainerCreate_NonPrivilegedRequestNotGated(t *testing.T) {
	// With the gate closed but no Privileged/CapAdd in the request, the
	// gate must let the request through. It will subsequently fail on
	// image lookup (because the zero-value client has no real backend)
	// — we just need to confirm we did NOT get the 403 short-circuit.
	s := gateTestServer(false)
	body := createRequest{
		Image:      "alpine:3.20",
		HostConfig: &hostConfig{Privileged: false},
	}
	w := postCreate(t, s, body)
	if w.Code == http.StatusForbidden {
		t.Errorf("non-privileged request was 403'd: %s", w.Body.String())
	}
}

func TestHandleContainerCreate_NilHostConfigNotGated(t *testing.T) {
	// docker run without --privileged sends a HostConfig with zero
	// values, but a hand-crafted client could omit HostConfig entirely.
	// The gate must not deref a nil HostConfig.
	s := gateTestServer(false)
	body := createRequest{Image: "alpine:3.20"}
	w := postCreate(t, s, body)
	if w.Code == http.StatusForbidden {
		t.Errorf("nil-HostConfig request was 403'd: %s", w.Body.String())
	}
}

func TestHandleContainerCreate_PrivilegedAllowedWhenGateOpen(t *testing.T) {
	// Mirror image: gate=true must NOT 403, even with Privileged=true.
	// (The handler will still fail later on image lookup against the
	// zero-value client, but not with 403.)
	s := gateTestServer(true)
	body := createRequest{
		Image:      "alpine:3.20",
		HostConfig: &hostConfig{Privileged: true, CapAdd: []string{"SYS_ADMIN"}},
	}
	w := postCreate(t, s, body)
	if w.Code == http.StatusForbidden {
		t.Errorf("gate=true rejected privileged request: %s", w.Body.String())
	}
}
