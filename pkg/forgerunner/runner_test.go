package forgerunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const testWorkflow = `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: Hello
        run: echo hello
`

// newTestForge creates an httptest server that handles ConnectRPC methods.
func newTestForge(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for method, handler := range handlers {
		path := fmt.Sprintf("/api/actions/runner.v1.RunnerService/%s", method)
		mux.HandleFunc(path, handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func jsonResponse(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, body)
}

// allHandlers returns a handler map with Register, Declare, FetchTask,
// UpdateTask, and UpdateLog pre-configured. Override individual methods
// by setting them after calling this.
func allHandlers(t *testing.T, workflowYAML string) map[string]http.HandlerFunc {
	t.Helper()
	payload := base64.StdEncoding.EncodeToString([]byte(workflowYAML))
	var fetched atomic.Bool

	return map[string]http.HandlerFunc{
		"Register": func(w http.ResponseWriter, _ *http.Request) {
			jsonResponse(w, `{"runner":{"id":1,"uuid":"u-1","name":"test","token":"tok-1"}}`)
		},
		"Declare": func(w http.ResponseWriter, _ *http.Request) {
			jsonResponse(w, `{}`)
		},
		"FetchTask": func(w http.ResponseWriter, _ *http.Request) {
			if fetched.Swap(true) {
				// Only return the task once; subsequent calls return empty.
				jsonResponse(w, `{"tasksVersion":1}`)
				return
			}
			jsonResponse(w, fmt.Sprintf(
				`{"task":{"id":42,"uuid":"t-42","workflowPayload":%q,"context":{"github":{"repository":"org/repo","ref":"refs/heads/main"}}},"tasksVersion":1}`,
				payload,
			))
		},
		"UpdateTask": func(w http.ResponseWriter, _ *http.Request) {
			jsonResponse(w, `{}`)
		},
		"UpdateLog": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			if json.Unmarshal(body, &req) == nil {
				idx, _ := req["index"].(float64)
				rows, _ := req["rows"].([]any)
				ack := int(idx) + len(rows)
				jsonResponse(w, fmt.Sprintf(`{"ackIndex":%d}`, ack))
				return
			}
			jsonResponse(w, `{"ackIndex":0}`)
		},
	}
}

func TestNew_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "missing instance URL",
			cfg:     Config{Token: "tok"},
			wantErr: "instance URL is required",
		},
		{
			name:    "missing token",
			cfg:     Config{InstanceURL: "https://example.com"},
			wantErr: "registration token is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if err == nil {
				t.Fatal("expected error")
			}
			if got := err.Error(); got != "forgerunner: "+tt.wantErr {
				t.Errorf("error = %q, want suffix %q", got, tt.wantErr)
			}
		})
	}
}

func TestNew_Defaults(t *testing.T) {
	r, err := New(Config{
		InstanceURL: "https://example.com",
		Token:       "tok",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.cfg.Version != "ephemerd-dev" {
		t.Errorf("Version = %q, want ephemerd-dev", r.cfg.Version)
	}
	if len(r.cfg.Labels) != 1 || r.cfg.Labels[0] != "ubuntu-latest" {
		t.Errorf("Labels = %v, want [ubuntu-latest]", r.cfg.Labels)
	}
	if r.cfg.Name == "" {
		t.Error("Name should default to hostname")
	}
}

func TestRun_RegisterAndExecute(t *testing.T) {
	handlers := allHandlers(t, testWorkflow)

	// Verify registration request.
	handlers["Register"] = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("unmarshal register request: %v", err)
		}
		if req["name"] != "test-runner" {
			t.Errorf("name = %v, want test-runner", req["name"])
		}
		jsonResponse(w, `{"runner":{"id":1,"uuid":"u-1","name":"test-runner","token":"tok-1"}}`)
	}

	// Track UpdateTask calls.
	var updateCount atomic.Int32
	handlers["UpdateTask"] = func(w http.ResponseWriter, _ *http.Request) {
		updateCount.Add(1)
		jsonResponse(w, `{}`)
	}

	srv := newTestForge(t, handlers)
	r, err := New(Config{
		Platform:    "forgejo",
		InstanceURL: srv.URL,
		Token:       "reg-token",
		Name:        "test-runner",
		Version:     "test/v1",
		Labels:      []string{"ubuntu-latest"},
		HTTPClient:  srv.Client(),
		Log:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = r.Run(context.Background())
	// `echo hello` should succeed — no error expected.
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// UpdateTask should have been called at least twice (start + completion).
	if n := updateCount.Load(); n < 2 {
		t.Errorf("UpdateTask called %d times, want >= 2", n)
	}
}

func TestRun_RegisterFailure(t *testing.T) {
	srv := newTestForge(t, map[string]http.HandlerFunc{
		"Register": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			jsonResponse(w, `{"code":"unauthenticated","message":"bad token"}`)
		},
	})

	r, err := New(Config{
		Platform:    "gitea",
		InstanceURL: srv.URL,
		Token:       "bad-token",
		HTTPClient:  srv.Client(),
		Log:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = r.Run(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRun_StepFailure(t *testing.T) {
	failWorkflow := `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: Fail
        run: exit 1
`
	handlers := allHandlers(t, failWorkflow)

	var lastResult float64
	handlers["UpdateTask"] = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if json.Unmarshal(body, &req) == nil {
			if state, ok := req["state"].(map[string]any); ok {
				if result, ok := state["result"].(float64); ok {
					lastResult = result
				}
			}
		}
		jsonResponse(w, `{}`)
	}

	srv := newTestForge(t, handlers)
	r, err := New(Config{
		InstanceURL: srv.URL,
		Token:       "tok",
		HTTPClient:  srv.Client(),
		Log:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = r.Run(context.Background())
	if err == nil {
		t.Fatal("expected error from failing step")
	}
	// Result 2 = failure
	if lastResult != 2 {
		t.Errorf("last UpdateTask result = %v, want 2 (failure)", lastResult)
	}
}

func TestPoll_ContextCancellation(t *testing.T) {
	fetchCount := 0
	srv := newTestForge(t, map[string]http.HandlerFunc{
		"Register": func(w http.ResponseWriter, _ *http.Request) {
			jsonResponse(w, `{"runner":{"id":1,"uuid":"u","name":"n","token":"t"}}`)
		},
		"Declare": func(w http.ResponseWriter, _ *http.Request) {
			jsonResponse(w, `{}`)
		},
		"FetchTask": func(w http.ResponseWriter, _ *http.Request) {
			fetchCount++
			jsonResponse(w, `{"tasksVersion":0}`)
		},
	})

	r, err := New(Config{
		InstanceURL: srv.URL,
		Token:       "tok",
		HTTPClient:  srv.Client(),
		Log:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err = r.Run(ctx)
	if err == nil || err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
	if fetchCount == 0 {
		t.Error("expected at least one FetchTask call")
	}
}

func TestPoll_RetryOnError(t *testing.T) {
	callCount := 0
	handlers := allHandlers(t, testWorkflow)

	handlers["FetchTask"] = func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		if callCount == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			jsonResponse(w, `{"code":"internal","message":"temporary"}`)
			return
		}
		payload := base64.StdEncoding.EncodeToString([]byte(testWorkflow))
		jsonResponse(w, fmt.Sprintf(
			`{"task":{"id":1,"workflowPayload":%q,"context":{"github":{"repository":"org/repo"}}},"tasksVersion":1}`,
			payload,
		))
	}

	srv := newTestForge(t, handlers)
	r, err := New(Config{
		InstanceURL: srv.URL,
		Token:       "tok",
		HTTPClient:  srv.Client(),
		Log:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = r.Run(context.Background())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 FetchTask calls (retry), got %d", callCount)
	}
}
