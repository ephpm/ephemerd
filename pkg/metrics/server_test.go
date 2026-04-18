package metrics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// freePort finds an available TCP port by binding to :0 and releasing it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}
	return port
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestServe_ReturnsMetrics(t *testing.T) {
	port := freePort(t)

	cleanup := Serve(context.Background(), ServerConfig{
		Port: port,
		Path: "/metrics",
		Log:  testLogger(),
	})
	defer cleanup()

	// Give the server a moment to start listening
	var resp *http.Response
	var lastErr error
	for i := 0; i < 20; i++ {
		resp, lastErr = http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		if lastErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("GET /metrics failed after retries: %v", lastErr)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") && !strings.Contains(ct, "text/openmetrics") {
		t.Errorf("Content-Type = %q, want prometheus text format", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if len(body) == 0 {
		t.Error("response body is empty, expected prometheus metrics")
	}
}

func TestServe_CleanupShutsDownServer(t *testing.T) {
	port := freePort(t)

	cleanup := Serve(context.Background(), ServerConfig{
		Port: port,
		Path: "/metrics",
		Log:  testLogger(),
	})

	// Wait for server to start
	var lastErr error
	for i := 0; i < 20; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Logf("closing response body: %v", closeErr)
			}
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("server did not start: %v", lastErr)
	}

	// Call cleanup to shut down
	cleanup()

	// Give shutdown a moment to complete
	time.Sleep(100 * time.Millisecond)

	// Server should no longer accept connections
	_, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err == nil {
		t.Error("expected connection error after cleanup, but request succeeded")
	}
}

func TestServe_CustomPath(t *testing.T) {
	port := freePort(t)

	cleanup := Serve(context.Background(), ServerConfig{
		Port: port,
		Path: "/custom-metrics",
		Log:  testLogger(),
	})
	defer cleanup()

	// Wait for server to start
	var resp *http.Response
	var lastErr error
	for i := 0; i < 20; i++ {
		resp, lastErr = http.Get(fmt.Sprintf("http://127.0.0.1:%d/custom-metrics", port))
		if lastErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("GET /custom-metrics failed: %v", lastErr)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// The default /metrics path should 404
	resp404, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer func() {
		if err := resp404.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp404.StatusCode != http.StatusNotFound {
		t.Errorf("/metrics status = %d, want %d (should not be registered)", resp404.StatusCode, http.StatusNotFound)
	}
}

func TestServe_Port0_PicksRandomPort(t *testing.T) {
	// Port 0 should cause the OS to pick a random port.
	// Since Serve() doesn't return the actual port, we can only verify
	// it doesn't panic or error — the server starts in a goroutine.
	cleanup := Serve(context.Background(), ServerConfig{
		Port: 0,
		Path: "/metrics",
		Log:  testLogger(),
	})
	defer cleanup()

	// If we got here without panic, the server accepted port 0.
	// We can't easily connect since we don't know the actual port,
	// but we verify cleanup works without error.
	cleanup()
}
