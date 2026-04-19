package forgejo

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

	"github.com/ephpm/ephemerd/pkg/forgerpc"
	"github.com/ephpm/ephemerd/pkg/providers"
)

func TestNew_RequiresInstanceURL(t *testing.T) {
	_, err := New(Config{Token: "tok", Log: slog.Default()})
	if err == nil {
		t.Fatal("expected error for missing instance_url")
	}
}

func TestNew_RequiresToken(t *testing.T) {
	_, err := New(Config{InstanceURL: "https://codeberg.org", Log: slog.Default()})
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestNew_Valid(t *testing.T) {
	p, err := New(Config{InstanceURL: "https://codeberg.org", Token: "tok", Log: slog.Default()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil || p.rpc == nil {
		t.Fatal("expected non-nil provider with rpc client")
	}
}

func TestName(t *testing.T) {
	if (&Provider{}).Name() != "forgejo" {
		t.Error("Name() != forgejo")
	}
}

func TestDefaultImage(t *testing.T) {
	if got := (&Provider{}).DefaultImage(); got != "data.forgejo.org/forgejo/runner:12" {
		t.Errorf("DefaultImage() = %q", got)
	}
}

func TestDefaultJobImage_Default(t *testing.T) {
	if got := (&Provider{}).DefaultJobImage(); got != "docker.io/gitea/runner-images:ubuntu-24.04" {
		t.Errorf("DefaultJobImage() = %q", got)
	}
}

func TestDefaultJobImage_Override(t *testing.T) {
	p := &Provider{cfg: Config{JobImage: "custom/image:latest"}}
	if p.DefaultJobImage() != "custom/image:latest" {
		t.Errorf("DefaultJobImage() = %q", p.DefaultJobImage())
	}
}

func TestClaimJob_EnvVars(t *testing.T) {
	p := &Provider{
		cfg:      Config{InstanceURL: "https://codeberg.org", Token: "reg-token"},
		runnerID: 42,
	}
	claim, err := p.ClaimJob(context.Background(), &providers.JobEvent{Repo: "myorg/myrepo", JobID: 1}, "runner-1", nil)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if claim.Env["FORGEJO_INSTANCE_URL"] != "https://codeberg.org" {
		t.Errorf("FORGEJO_INSTANCE_URL = %q", claim.Env["FORGEJO_INSTANCE_URL"])
	}
	if claim.Env["FORGEJO_RUNNER_TOKEN"] != "reg-token" {
		t.Errorf("FORGEJO_RUNNER_TOKEN = %q", claim.Env["FORGEJO_RUNNER_TOKEN"])
	}
	if len(claim.Entrypoint) == 0 {
		t.Error("Entrypoint is empty, expected self-registration command")
	}
}

func TestClaimJob_TaskUUID(t *testing.T) {
	p := &Provider{cfg: Config{InstanceURL: "https://codeberg.org", Token: "tok"}}
	task := &forgerpc.Task{ID: 10, UUID: "task-uuid-xyz"}
	claim, err := p.ClaimJob(context.Background(), &providers.JobEvent{Raw: task}, "r1", nil)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if claim.Env["FORGEJO_TASK_UUID"] != "task-uuid-xyz" {
		t.Errorf("FORGEJO_TASK_UUID = %q", claim.Env["FORGEJO_TASK_UUID"])
	}
}

func TestClaimJob_NoTaskUUID(t *testing.T) {
	p := &Provider{cfg: Config{InstanceURL: "https://codeberg.org", Token: "tok"}}
	claim, err := p.ClaimJob(context.Background(), &providers.JobEvent{}, "r1", nil)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if _, ok := claim.Env["FORGEJO_TASK_UUID"]; ok {
		t.Error("FORGEJO_TASK_UUID should not be set when Raw is nil")
	}
}

func TestReleaseJob_Noop(t *testing.T) {
	if err := (&Provider{}).ReleaseJob(context.Background(), &providers.Claim{}); err != nil {
		t.Fatalf("ReleaseJob: %v", err)
	}
}

func TestFetchJobImage_FromTask(t *testing.T) {
	workflow := "name: CI\njobs:\n  build:\n    runs-on: ubuntu-latest\n    env:\n      EPHEMERD_IMAGE: custom/runner:v3\n"
	task := &forgerpc.Task{ID: 1, WorkflowPayload: base64.StdEncoding.EncodeToString([]byte(workflow))}
	if img := (&Provider{}).FetchJobImage(context.Background(), &providers.JobEvent{Raw: task}); img != "custom/runner:v3" {
		t.Errorf("FetchJobImage() = %q", img)
	}
}

func TestFetchJobImage_NoTask(t *testing.T) {
	if img := (&Provider{}).FetchJobImage(context.Background(), &providers.JobEvent{}); img != "" {
		t.Errorf("FetchJobImage() = %q, want empty", img)
	}
}

func TestStop(t *testing.T) {
	if err := (&Provider{}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop nil cancel: %v", err)
	}
	_, cancel := context.WithCancel(context.Background())
	if err := (&Provider{cancel: cancel}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop with cancel: %v", err)
	}
}

func TestBuildLabels_Default(t *testing.T) {
	labels := (&Provider{}).buildLabels()
	if len(labels) != 1 || labels[0] != "ubuntu-latest:docker://docker.io/gitea/runner-images:ubuntu-24.04" {
		t.Errorf("buildLabels() = %v", labels)
	}
}

func TestBuildLabels_Custom(t *testing.T) {
	labels := (&Provider{cfg: Config{Labels: []string{"a", "b"}}}).buildLabels()
	if len(labels) != 2 {
		t.Errorf("buildLabels() = %v", labels)
	}
}

// --- Integration test with mock forge ---

func writeJSON(t *testing.T, w http.ResponseWriter, s string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprint(w, s); err != nil {
		t.Errorf("write: %v", err)
	}
}

func writeJSONf(t *testing.T, w http.ResponseWriter, format string, args ...any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		t.Errorf("write: %v", err)
	}
}

func mockForge(t *testing.T, tasks []mockTask) *httptest.Server {
	t.Helper()
	var taskIdx atomic.Int32
	mux := http.NewServeMux()
	prefix := "/api/actions/runner.v1.RunnerService"

	mux.HandleFunc(prefix+"/Register", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("register unmarshal: %v", err)
		}
		writeJSON(t, w, `{"runner":{"id":1,"uuid":"forge-uuid","name":"test","token":"forge-token"}}`)
	})
	mux.HandleFunc(prefix+"/Declare", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{}`)
	})
	mux.HandleFunc(prefix+"/FetchTask", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-runner-uuid") == "" {
			t.Error("FetchTask: missing x-runner-uuid")
		}
		idx := int(taskIdx.Load())
		if idx < len(tasks) {
			taskIdx.Add(1)
			tk := tasks[idx]
			writeJSONf(t, w, `{"task":{"id":%d,"uuid":%q,"workflowPayload":%q,"context":%s},"tasksVersion":%d}`,
				tk.id, tk.uuid, tk.payload, tk.context, idx+1)
		} else {
			writeJSONf(t, w, `{"tasksVersion":%d}`, len(tasks))
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type mockTask struct {
	id      int64
	uuid    string
	payload string
	context string
}

func TestStart_RegisterAndPoll(t *testing.T) {
	workflow := base64.StdEncoding.EncodeToString([]byte("name: CI\njobs:\n  build:\n    runs-on: ubuntu-latest\n"))
	srv := mockForge(t, []mockTask{
		{id: 77, uuid: "task-uuid-1", payload: workflow, context: `{"github":{"repository":"myorg/myrepo"}}`},
	})

	p, err := New(Config{InstanceURL: srv.URL, Token: "reg-tok", HTTPClient: srv.Client(), Log: slog.Default()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := p.Start(ctx, providers.PollConfig{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if p.runnerUUID != "forge-uuid" || p.runnerToken != "forge-token" {
		t.Errorf("registration: uuid=%q token=%q", p.runnerUUID, p.runnerToken)
	}

	select {
	case ev := <-events:
		if ev.Action != "queued" || ev.JobID != 77 || ev.Repo != "myorg/myrepo" {
			t.Errorf("event = %+v", ev)
		}
		task, ok := ev.Raw.(*forgerpc.Task)
		if !ok || task.UUID != "task-uuid-1" {
			t.Errorf("task UUID = %v", ev.Raw)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}

	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-timer.C:
			t.Fatal("events channel not closed after Stop")
		}
	}
}

func TestStart_RegisterFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/actions/runner.v1.RunnerService/Register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(t, w, `{"code":"unauthenticated","message":"bad token"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, _ := New(Config{InstanceURL: srv.URL, Token: "bad", HTTPClient: srv.Client(), Log: slog.Default()})
	if _, err := p.Start(context.Background(), providers.PollConfig{}); err == nil {
		t.Fatal("expected error")
	}
}
