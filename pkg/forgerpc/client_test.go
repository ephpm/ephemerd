package forgerpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestServer creates an httptest server that routes ConnectRPC methods.
func newTestServer(t *testing.T, handlers map[string]http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	for method, handler := range handlers {
		path := fmt.Sprintf("%s/%s/%s", apiPrefix, servicePath, method)
		mux.HandleFunc(path, handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, srv.Client())
	return srv, client
}

func writeJSON(t *testing.T, w http.ResponseWriter, s string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprint(w, s); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func writeJSONf(t *testing.T, w http.ResponseWriter, format string, args ...any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestRegister_Success(t *testing.T) {
	_, client := newTestServer(t, map[string]http.HandlerFunc{
		"Register": func(w http.ResponseWriter, r *http.Request) {
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if v := r.Header.Get("Connect-Protocol-Version"); v != "1" {
				t.Errorf("Connect-Protocol-Version = %q, want 1", v)
			}
			if h := r.Header.Get("x-runner-uuid"); h != "" {
				t.Errorf("unexpected x-runner-uuid on Register: %q", h)
			}

			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}
			if req["name"] != "test-runner" {
				t.Errorf("name = %v, want test-runner", req["name"])
			}
			if req["token"] != "reg-token" {
				t.Errorf("token = %v, want reg-token", req["token"])
			}

			writeJSON(t, w, `{"runner":{"id":42,"uuid":"uuid-abc","name":"test-runner","token":"persistent-tok"}}`)
		},
	})

	runner, err := client.Register(context.Background(), "test-runner", "reg-token", "v0.1.0", []string{
		"self-hosted",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if runner.ID != 42 {
		t.Errorf("ID = %d, want 42", runner.ID)
	}
	if runner.UUID != "uuid-abc" {
		t.Errorf("UUID = %q, want uuid-abc", runner.UUID)
	}
	if runner.Token != "persistent-tok" {
		t.Errorf("Token = %q, want persistent-tok", runner.Token)
	}
	if client.UUID() != "uuid-abc" {
		t.Errorf("client UUID = %q, want uuid-abc", client.UUID())
	}
	if client.Token() != "persistent-tok" {
		t.Errorf("client Token = %q, want persistent-tok", client.Token())
	}
}

func TestRegister_Int64AsString(t *testing.T) {
	_, client := newTestServer(t, map[string]http.HandlerFunc{
		"Register": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, `{"runner":{"id":"999","uuid":"u","name":"n","token":"t"}}`)
		},
	})

	runner, err := client.Register(context.Background(), "r", "tok", "v1", nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if runner.ID != 999 {
		t.Errorf("ID = %d, want 999 (parsed from string)", runner.ID)
	}
}

func TestRegister_ServerError(t *testing.T) {
	_, client := newTestServer(t, map[string]http.HandlerFunc{
		"Register": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(t, w, `{"code":"unauthenticated","message":"invalid registration token"}`)
		},
	})

	_, err := client.Register(context.Background(), "r", "bad", "v1", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		if rpcErr.Code != "unauthenticated" {
			t.Errorf("Code = %q, want unauthenticated", rpcErr.Code)
		}
	}
}

func TestDeclare(t *testing.T) {
	_, client := newTestServer(t, map[string]http.HandlerFunc{
		"Declare": func(w http.ResponseWriter, r *http.Request) {
			if u := r.Header.Get("x-runner-uuid"); u != "my-uuid" {
				t.Errorf("x-runner-uuid = %q, want my-uuid", u)
			}
			if tok := r.Header.Get("x-runner-token"); tok != "my-token" {
				t.Errorf("x-runner-token = %q, want my-token", tok)
			}
			writeJSON(t, w, `{}`)
		},
	})

	client.SetAuth("my-uuid", "my-token")
	if err := client.Declare(context.Background(), []string{
		"ubuntu-latest:docker://node:20",
	}); err != nil {
		t.Fatalf("Declare: %v", err)
	}
}

func TestFetchTask_WithTask(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("name: CI\njobs:\n  build:\n    runs-on: ubuntu-latest\n"))
	taskCtx := `{"github":{"repository":"myorg/myrepo","ref":"refs/heads/main"}}`

	_, client := newTestServer(t, map[string]http.HandlerFunc{
		"FetchTask": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("unmarshal: %v", err)
			}
			if _, ok := req["tasksVersion"]; !ok {
				t.Error("missing tasksVersion in request")
			}
			writeJSONf(t, w, `{"task":{"id":77,"workflowPayload":%q,"context":%s},"tasksVersion":5}`, payload, taskCtx)
		},
	})

	client.SetAuth("u", "t")
	result, err := client.FetchTask(context.Background(), 0)
	if err != nil {
		t.Fatalf("FetchTask: %v", err)
	}
	if result.Task == nil {
		t.Fatal("expected task, got nil")
	}
	if result.Task.ID != 77 {
		t.Errorf("Task.ID = %d, want 77", result.Task.ID)
	}
	if result.TasksVersion != 5 {
		t.Errorf("TasksVersion = %d, want 5", result.TasksVersion)
	}
}

func TestFetchTask_NoTask(t *testing.T) {
	_, client := newTestServer(t, map[string]http.HandlerFunc{
		"FetchTask": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, `{"tasksVersion":3}`)
		},
	})

	client.SetAuth("u", "t")
	result, err := client.FetchTask(context.Background(), 0)
	if err != nil {
		t.Fatalf("FetchTask: %v", err)
	}
	if result.Task != nil {
		t.Errorf("expected nil task, got %+v", result.Task)
	}
	if result.TasksVersion != 3 {
		t.Errorf("TasksVersion = %d, want 3", result.TasksVersion)
	}
}

func TestFetchTask_AuthHeaders(t *testing.T) {
	_, client := newTestServer(t, map[string]http.HandlerFunc{
		"FetchTask": func(w http.ResponseWriter, r *http.Request) {
			if u := r.Header.Get("x-runner-uuid"); u != "runner-uuid" {
				t.Errorf("x-runner-uuid = %q, want runner-uuid", u)
			}
			if tok := r.Header.Get("x-runner-token"); tok != "runner-token" {
				t.Errorf("x-runner-token = %q, want runner-token", tok)
			}
			writeJSON(t, w, `{"tasksVersion":0}`)
		},
	})

	client.SetAuth("runner-uuid", "runner-token")
	if _, err := client.FetchTask(context.Background(), 0); err != nil {
		t.Fatalf("FetchTask: %v", err)
	}
}

func TestTask_Repo(t *testing.T) {
	tests := []struct {
		name    string
		context string
		want    string
	}{
		{"nested under github", `{"github":{"repository":"org/repo"}}`, "org/repo"},
		{"direct field", `{"repository":"direct/repo"}`, "direct/repo"},
		{"empty context", `{}`, ""},
		{"null context", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := &Task{Context: json.RawMessage(tt.context)}
			if got := task.Repo(); got != tt.want {
				t.Errorf("Repo() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTask_WorkflowYAML(t *testing.T) {
	original := "name: CI\non: push\n"

	t.Run("standard base64", func(t *testing.T) {
		task := &Task{WorkflowPayload: base64.StdEncoding.EncodeToString([]byte(original))}
		got, err := task.WorkflowYAML()
		if err != nil {
			t.Fatalf("WorkflowYAML: %v", err)
		}
		if string(got) != original {
			t.Errorf("got %q, want %q", string(got), original)
		}
	})
	t.Run("no padding", func(t *testing.T) {
		task := &Task{WorkflowPayload: base64.RawStdEncoding.EncodeToString([]byte(original))}
		got, err := task.WorkflowYAML()
		if err != nil {
			t.Fatalf("WorkflowYAML: %v", err)
		}
		if string(got) != original {
			t.Errorf("got %q, want %q", string(got), original)
		}
	})
	t.Run("empty payload", func(t *testing.T) {
		task := &Task{}
		got, err := task.WorkflowYAML()
		if err != nil {
			t.Fatalf("WorkflowYAML: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil, got %q", string(got))
		}
	})
}

func TestTask_ContainerImage(t *testing.T) {
	tests := []struct {
		name, workflow, want string
	}{
		{
			name:     "container mapping form",
			workflow: "name: CI\njobs:\n  build:\n    runs-on: ubuntu-latest\n    container:\n      image: custom/image:v2\n    steps:\n      - run: echo hello\n",
			want:     "custom/image:v2",
		},
		{
			name:     "container string shorthand",
			workflow: "name: CI\njobs:\n  build:\n    runs-on: ubuntu-latest\n    container: custom/short:v1\n    steps:\n      - run: echo hello\n",
			want:     "custom/short:v1",
		},
		{
			name:     "not set",
			workflow: "name: CI\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hello\n",
			want:     "",
		},
		{name: "empty payload", workflow: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var payload string
			if tt.workflow != "" {
				payload = base64.StdEncoding.EncodeToString([]byte(tt.workflow))
			}
			task := &Task{WorkflowPayload: payload}
			if got := task.ContainerImage(); got != tt.want {
				t.Errorf("ContainerImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRegistrationToken_PostFirst(t *testing.T) {
	// Gitea 1.24+: POST returns token.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/org/repo/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeJSON(t, w, `{"token":"post-token-abc"}`)
			return
		}
		writeJSON(t, w, `{"token":"get-token-xyz"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, srv.Client())

	tok, err := client.RegistrationToken(context.Background(), "api-key", "org", "repo")
	if err != nil {
		t.Fatalf("RegistrationToken: %v", err)
	}
	if tok != "post-token-abc" {
		t.Errorf("token = %q, want post-token-abc (POST should win)", tok)
	}
}

func TestRegistrationToken_GetFallback(t *testing.T) {
	// Gitea 1.22-1.23: POST returns 405, GET returns token.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/org/repo/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(t, w, `{"token":"get-fallback-token"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, srv.Client())

	tok, err := client.RegistrationToken(context.Background(), "api-key", "org", "repo")
	if err != nil {
		t.Fatalf("RegistrationToken: %v", err)
	}
	if tok != "get-fallback-token" {
		t.Errorf("token = %q, want get-fallback-token", tok)
	}
}

func TestRegistrationToken_NeitherWorks(t *testing.T) {
	// Gitea <1.22: endpoint doesn't exist.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/org/repo/actions/runners/registration-token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, srv.Client())

	_, err := client.RegistrationToken(context.Background(), "api-key", "org", "repo")
	if err == nil {
		t.Fatal("expected error for pre-1.22 Gitea")
	}
}

func TestDeclareLabels(t *testing.T) {
	tests := []struct {
		input []string
		want  []string
	}{
		{[]string{"ubuntu-latest:docker://node:20"}, []string{"ubuntu-latest"}},
		{[]string{"self-hosted", "linux"}, []string{"self-hosted", "linux"}},
		{[]string{"custom:docker://img:v1", "plain"}, []string{"custom", "plain"}},
	}
	for _, tt := range tests {
		got := DeclareLabels(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("DeclareLabels(%v) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("DeclareLabels(%v)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestRPCError_Format(t *testing.T) {
	err := &RPCError{HTTPStatus: 401, Code: "unauthenticated", Message: "bad token"}
	if got := err.Error(); got != "rpc error: unauthenticated: bad token (HTTP 401)" {
		t.Errorf("Error() = %q", got)
	}
	err2 := &RPCError{HTTPStatus: 500, Message: "internal server error"}
	if got := err2.Error(); got != "rpc error: HTTP 500: internal server error" {
		t.Errorf("Error() = %q", got)
	}
}

func TestParseFlexInt64(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  int64
	}{
		{`42`, 42}, {`"42"`, 42}, {`0`, 0}, {`"0"`, 0}, {`null`, 0},
	} {
		got, err := parseFlexInt64(json.RawMessage(tt.input))
		if err != nil {
			t.Errorf("parseFlexInt64(%s): %v", tt.input, err)
		} else if got != tt.want {
			t.Errorf("parseFlexInt64(%s) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
