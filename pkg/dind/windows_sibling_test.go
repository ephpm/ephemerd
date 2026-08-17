package dind

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// setPlatformGOOS overrides the package's platform variable for one test and
// restores it afterwards, so both the Linux and the Windows branch of
// checkWindowsSiblingGate are exercised on whichever host runs the suite.
func setPlatformGOOS(t *testing.T, goos string) {
	t.Helper()
	prev := platformGOOS
	platformGOOS = goos
	t.Cleanup(func() { platformGOOS = prev })
}

func TestCheckWindowsSiblingGate_BlocksOnWindows(t *testing.T) {
	msg, blocked := checkWindowsSiblingGate("windows")
	if !blocked {
		t.Fatal("blocked = false on windows, want true")
	}
	// The message is the only diagnosis the job ever gets — the Docker CLI
	// prints it as "Error response from daemon: <message>" and the Actions
	// runner surfaces that in the log. Assert it names both the cause and
	// the way out, so a future edit cannot quietly reduce it to "not
	// supported".
	for _, want := range []string{
		"Windows host",
		"overlayfs",
		"container:",
		"[runner.images.<repo>].windows",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q: %s", want, msg)
		}
	}
}

func TestCheckWindowsSiblingGate_AllowsElsewhere(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			msg, blocked := checkWindowsSiblingGate(goos)
			if blocked {
				t.Errorf("blocked = true on %s; msg=%q", goos, msg)
			}
			if msg != "" {
				t.Errorf("msg = %q, want empty when not blocked", msg)
			}
		})
	}
}

// The gate has to fire on the request, before the handler reaches containerd —
// otherwise the failure surfaces as a missing-snapshotter error from three
// layers down. A nil client here proves nothing downstream was consulted.
func TestHandleContainerCreate_RejectedOnWindowsHost(t *testing.T) {
	setPlatformGOOS(t, "windows")
	s := gateTestServer(true) // privileged gate open, so this is the only gate left
	w := postCreate(t, s, createRequest{Image: "golang:1.26.6-windowsservercore-ltsc2025"})

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a Docker error object: %v (%s)", err, w.Body.String())
	}
	if !strings.Contains(body["message"], "cannot create containers on a Windows host") {
		t.Errorf("message = %q", body["message"])
	}
}

// The privileged gate stays first: an elevated request on Windows is still a
// 403, not a 501. Ordering matters because the two carry different advice.
func TestHandleContainerCreate_PrivilegedGateStillWinsOnWindows(t *testing.T) {
	setPlatformGOOS(t, "windows")
	s := gateTestServer(false)
	w := postCreate(t, s, createRequest{
		Image:      "alpine:3.20",
		HostConfig: &hostConfig{Privileged: true},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// Request-shape validation also stays ahead of the platform gate.
func TestHandleContainerCreate_MissingImageStillBadRequestOnWindows(t *testing.T) {
	setPlatformGOOS(t, "windows")
	s := gateTestServer(true)
	w := postCreate(t, s, createRequest{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// On a Linux host nothing changes: the handler falls through the gate to the
// nil-client check exactly as before.
func TestHandleContainerCreate_NotGatedOnLinuxHost(t *testing.T) {
	setPlatformGOOS(t, "linux")
	s := gateTestServer(true)
	w := postCreate(t, s, createRequest{Image: "alpine:3.20"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (nil containerd client); body=%s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("Windows host")) {
		t.Errorf("Linux host got the Windows rejection: %s", w.Body.String())
	}
}

// Guards the HTTP surface end to end (mux → handler → status), not just the
// handler function, since the runner talks to the mux.
func TestContainerCreateOverHTTP_RejectedOnWindowsHost(t *testing.T) {
	s := newTestServer(t)
	setPlatformGOOS(t, "windows")
	client := dialServer(s)

	body, err := json.Marshal(map[string]any{"Image": "alpine:latest"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := client.Post("http://docker/containers/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /containers/create: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}
