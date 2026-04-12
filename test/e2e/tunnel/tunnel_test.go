//go:build e2e

package tunnel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	githubpkg "github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/tunnel"
)

const (
	testOwner  = "ephpm"
	testRepo   = "ephemerd"
	testSecret = "ephemerd-e2e-test-secret"
)

// TestLocaltunnelWebhook verifies the full webhook flow:
// 1. Create a localtunnel to get a public URL
// 2. Serve a webhook handler on it
// 3. Register a webhook on GitHub pointing at the tunnel
// 4. Ping the webhook via GitHub API
// 5. Verify the ping event arrives through the tunnel
// 6. Clean up the webhook on exit
func TestLocaltunnelWebhook(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// 1. Create localtunnel
	lt := tunnel.NewLocalTunnel("")
	ln, err := lt.Listen(ctx)
	if err != nil {
		t.Fatalf("localtunnel listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	publicURL := lt.PublicURL()
	webhookURL := publicURL + "/webhook"
	t.Logf("tunnel ready: %s", webhookURL)

	// 2. Serve webhook handler
	var (
		mu       sync.Mutex
		received []webhookEvent
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Logf("webhook: failed to read body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Verify signature
		sig := r.Header.Get("X-Hub-Signature-256")
		if !verifyWebhookSignature(body, sig, testSecret) {
			t.Logf("webhook: signature verification failed (sig=%s)", sig)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		eventType := r.Header.Get("X-GitHub-Event")
		t.Logf("webhook: received event=%s", eventType)

		mu.Lock()
		received = append(received, webhookEvent{
			Event: eventType,
			Body:  body,
		})
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{Handler: mux}
	go func() {
		if err := server.Serve(ln); err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer func() { _ = server.Shutdown(context.Background()) }()

	// 3. Register webhook on GitHub
	gh, err := githubpkg.New(githubpkg.Config{
		Token: token,
		Owner: testOwner,
		Repos: []string{testRepo},
		Log:   log,
	})
	if err != nil {
		t.Fatalf("creating github client: %v", err)
	}

	hooks, err := gh.RegisterWebhooks(ctx, webhookURL, testSecret)
	if err != nil {
		t.Fatalf("registering webhook: %v", err)
	}
	t.Logf("registered %d webhook(s)", len(hooks))

	// Always clean up
	defer func() {
		t.Log("deregistering webhooks")
		gh.DeregisterWebhooks(context.Background(), hooks)
	}()

	// 4. Ping the webhook via GitHub API — retry since the free tunnel may drop connections
	pingHooks := func() {
		for _, hook := range hooks {
			t.Logf("pinging webhook %d", hook.HookID)
			if err := gh.PingWebhook(ctx, hook); err != nil {
				t.Logf("ping failed (will retry): %v", err)
			}
		}
	}
	pingHooks()

	// 5. Wait for the ping to arrive, re-pinging periodically
	deadline := time.After(60 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	reping := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	defer reping.Stop()

	for {
		select {
		case <-deadline:
			mu.Lock()
			count := len(received)
			mu.Unlock()
			t.Fatalf("timed out waiting for webhook ping (received %d events)", count)
		case <-reping.C:
			t.Log("re-pinging webhook...")
			pingHooks()
		case <-ticker.C:
			mu.Lock()
			count := len(received)
			mu.Unlock()
			if count > 0 {
				mu.Lock()
				ev := received[0]
				mu.Unlock()
				t.Logf("received webhook event: %s", ev.Event)
				if ev.Event != "ping" {
					t.Errorf("expected ping event, got %s", ev.Event)
				}

				// Parse the ping payload to verify it has the expected fields
				var ping struct {
					Zen    string `json:"zen"`
					HookID int64  `json:"hook_id"`
				}
				if err := json.Unmarshal(ev.Body, &ping); err != nil {
					t.Errorf("failed to parse ping payload: %v", err)
				} else {
					t.Logf("ping zen=%q hook_id=%d", ping.Zen, ping.HookID)
				}
				return
			}
		}
	}
}

type webhookEvent struct {
	Event string
	Body  []byte
}

func verifyWebhookSignature(body []byte, signature string, secret string) bool {
	if len(signature) < 7 || signature[:7] != "sha256=" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature[7:]), []byte(expected))
}

// listenWithRetry attempts to establish a localtunnel connection with retries.
// The free loca.lt server is unreliable and Listen can hang indefinitely,
// so each attempt gets a short timeout and we retry until the deadline.
func listenWithRetry(t *testing.T, deadline time.Duration) (net.Listener, *tunnel.LocalTunnel) {
	t.Helper()
	start := time.Now()
	for attempt := 1; time.Since(start) < deadline; attempt++ {
		lt := tunnel.NewLocalTunnel("")

		type result struct {
			ln  net.Listener
			err error
		}
		ch := make(chan result, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		go func() {
			ln, err := lt.Listen(ctx)
			ch <- result{ln, err}
		}()

		select {
		case r := <-ch:
			cancel()
			if r.err != nil {
				t.Logf("attempt %d: listen failed: %v", attempt, r.err)
				continue
			}
			t.Logf("attempt %d: tunnel ready: %s", attempt, lt.PublicURL())
			return r.ln, lt
		case <-ctx.Done():
			cancel()
			t.Logf("attempt %d: timed out after 10s, retrying...", attempt)
		}
	}
	t.Fatalf("failed to establish tunnel after %s", deadline)
	return nil, nil
}

// TestLocaltunnelHTTP verifies the tunnel works for basic HTTP without GitHub.
// This is a fast sanity check that doesn't need a GitHub token.
func TestLocaltunnelHTTP(t *testing.T) {
	ln, lt := listenWithRetry(t, 90*time.Second)
	defer func() { _ = ln.Close() }()

	publicURL := lt.PublicURL()
	t.Logf("tunnel URL: %s", publicURL)

	// Serve a simple handler
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok"}`)
	})

	server := &http.Server{Handler: mux}
	go func() {
		if err := server.Serve(ln); err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer func() { _ = server.Shutdown(context.Background()) }()

	// Hit the public URL — retry on transient errors (502, 503, connection resets)
	client := &http.Client{Timeout: 15 * time.Second}
	var body []byte
	for attempt := 1; attempt <= 10; attempt++ {
		resp, err := client.Get(publicURL + "/healthz")
		if err != nil {
			t.Logf("attempt %d: GET error: %v", attempt, err)
			time.Sleep(2 * time.Second)
			continue
		}
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Logf("attempt %d: error reading body: %v", attempt, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Logf("error closing response body: %v", err)
		}
		if resp.StatusCode == http.StatusOK {
			t.Logf("attempt %d: success", attempt)
			break
		}
		t.Logf("attempt %d: status %d, retrying...", attempt, resp.StatusCode)
		body = nil
		time.Sleep(2 * time.Second)
	}
	if body == nil {
		t.Fatal("failed to get successful response after 10 attempts")
	}
	t.Logf("response: %s", string(body))

	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result.Status != "ok" {
		t.Errorf("status = %q, want %q", result.Status, "ok")
	}
}
