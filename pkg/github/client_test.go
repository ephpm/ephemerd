package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// --- verifySignature tests ---

func TestVerifySignature_Valid(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"action":"queued"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifySignature(body, sig, secret) {
		t.Error("expected valid signature to pass")
	}
}

func TestVerifySignature_Invalid(t *testing.T) {
	if verifySignature([]byte("body"), "sha256=deadbeef", "secret") {
		t.Error("expected invalid signature to fail")
	}
}

func TestVerifySignature_MissingPrefix(t *testing.T) {
	if verifySignature([]byte("body"), "deadbeef", "secret") {
		t.Error("expected signature without sha256= prefix to fail")
	}
}

func TestVerifySignature_Empty(t *testing.T) {
	if verifySignature([]byte("body"), "", "secret") {
		t.Error("expected empty signature to fail")
	}
}

func TestVerifySignature_ShortString(t *testing.T) {
	if verifySignature([]byte("body"), "sha25", "secret") {
		t.Error("expected short signature to fail")
	}
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	secret := "correct-secret"
	body := []byte(`test`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if verifySignature(body, sig, "wrong-secret") {
		t.Error("expected wrong secret to fail verification")
	}
}

func TestVerifySignature_EmptyBody(t *testing.T) {
	secret := "secret"
	body := []byte{}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifySignature(body, sig, secret) {
		t.Error("expected valid signature with empty body to pass")
	}
}

// --- isSelfHosted tests ---

func TestIsSelfHosted(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"with self-hosted", []string{"self-hosted", "linux", "x64"}, true},
		{"without self-hosted", []string{"ubuntu-latest"}, false},
		{"empty labels", nil, false},
		{"self-hosted only", []string{"self-hosted"}, true},
		{"case sensitive", []string{"Self-Hosted"}, false},
		{"self-hosted not first", []string{"linux", "self-hosted", "x64"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSelfHosted(tt.labels); got != tt.want {
				t.Errorf("isSelfHosted(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

// --- isTrackedRepo tests ---

func TestIsTrackedRepo(t *testing.T) {
	tests := []struct {
		name  string
		repos []string
		repo  string
		want  bool
	}{
		{"org level accepts all", nil, "any-repo", true},
		{"empty repos accepts all", []string{}, "any-repo", true},
		{"tracked repo", []string{"repo1", "repo2"}, "repo1", true},
		{"untracked repo", []string{"repo1", "repo2"}, "repo3", false},
		{"case sensitive", []string{"Repo1"}, "repo1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{cfg: Config{Repos: tt.repos}}
			if got := c.isTrackedRepo(tt.repo); got != tt.want {
				t.Errorf("isTrackedRepo(%q) = %v, want %v", tt.repo, got, tt.want)
			}
		})
	}
}

// --- IsOrgLevel tests ---

func TestIsOrgLevel(t *testing.T) {
	tests := []struct {
		name  string
		repos []string
		want  bool
	}{
		{"no repos", nil, true},
		{"empty repos", []string{}, true},
		{"with repos", []string{"repo1"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{cfg: Config{Repos: tt.repos}}
			if got := c.IsOrgLevel(); got != tt.want {
				t.Errorf("IsOrgLevel() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- parseEphemerdImage tests ---

func TestParseEphemerdImage(t *testing.T) {
	workflow := `
name: CI
on: push
jobs:
  build:
    runs-on: [self-hosted, linux]
    env:
      EPHEMERD_IMAGE: ghcr.io/myorg/custom:latest
    steps:
      - uses: actions/checkout@v4
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`

	tests := []struct {
		name    string
		jobName string
		jobID   int64
		want    string
	}{
		{
			name:    "job with EPHEMERD_IMAGE",
			jobName: "build",
			jobID:   1,
			want:    "ghcr.io/myorg/custom:latest",
		},
		{
			name:    "job without EPHEMERD_IMAGE",
			jobName: "test",
			jobID:   2,
			want:    "",
		},
		{
			name:    "unknown job ID",
			jobName: "",
			jobID:   999,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var jobs []*gh.WorkflowJob
			if tt.jobName != "" {
				jobs = []*gh.WorkflowJob{
					{ID: gh.Ptr(tt.jobID), Name: gh.Ptr(tt.jobName)},
				}
			}
			got := parseEphemerdImage(workflow, jobs, tt.jobID)
			if got != tt.want {
				t.Errorf("parseEphemerdImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseEphemerdImage_InvalidYAML(t *testing.T) {
	jobs := []*gh.WorkflowJob{{ID: gh.Ptr(int64(1)), Name: gh.Ptr("build")}}
	got := parseEphemerdImage("not: valid: yaml: {{", jobs, 1)
	if got != "" {
		t.Errorf("expected empty string for invalid YAML, got %q", got)
	}
}

func TestParseEphemerdImage_ExplicitJobName(t *testing.T) {
	// When job has explicit `name:` field that differs from the key
	workflow := `
jobs:
  my-build-job:
    name: Build Everything
    runs-on: [self-hosted, linux]
    env:
      EPHEMERD_IMAGE: ghcr.io/myorg/builder:v2
`
	jobs := []*gh.WorkflowJob{{ID: gh.Ptr(int64(1)), Name: gh.Ptr("Build Everything")}}
	got := parseEphemerdImage(workflow, jobs, 1)
	if got != "ghcr.io/myorg/builder:v2" {
		t.Errorf("parseEphemerdImage() = %q, want %q", got, "ghcr.io/myorg/builder:v2")
	}
}

// --- base64url tests ---

func TestBase64url(t *testing.T) {
	// Should not have trailing '=' padding
	result := base64url([]byte("test"))
	if strings.Contains(result, "=") {
		t.Errorf("base64url should not contain padding, got %q", result)
	}
	if result == "" {
		t.Error("base64url returned empty string")
	}
}

func TestBase64url_Empty(t *testing.T) {
	result := base64url([]byte{})
	if result != "" {
		t.Errorf("base64url of empty = %q, want empty", result)
	}
}

// --- WebhookHandler tests ---

func newTestClient(repos ...string) *Client {
	return &Client{
		cfg: Config{
			Owner: "testorg",
			Repos: repos,
			Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
}

func signPayload(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookHandler_MethodNotAllowed(t *testing.T) {
	c := newTestClient()
	handler, _ := c.WebhookHandler("secret")

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestWebhookHandler_InvalidSignature(t *testing.T) {
	c := newTestClient()
	handler, _ := c.WebhookHandler("secret")

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("{}"))
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("bad sig status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestWebhookHandler_NonWorkflowJobEvent(t *testing.T) {
	c := newTestClient()
	secret := "test-secret"
	handler, _ := c.WebhookHandler(secret)

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signPayload(body, secret))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("non-workflow_job status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestWebhookHandler_ValidWorkflowJobEvent(t *testing.T) {
	c := newTestClient("my-repo")
	secret := "test-secret"
	handler, events := c.WebhookHandler(secret)

	payload := map[string]any{
		"action": "queued",
		"workflow_job": map[string]any{
			"id":     123,
			"labels": []string{"self-hosted", "linux"},
		},
		"repository": map[string]any{
			"name": "my-repo",
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", signPayload(body, secret))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("valid webhook status = %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case evt := <-events:
		if evt.Action != "queued" {
			t.Errorf("event action = %q, want %q", evt.Action, "queued")
		}
		if evt.Repo != "my-repo" {
			t.Errorf("event repo = %q, want %q", evt.Repo, "my-repo")
		}
	default:
		t.Fatal("expected event on channel, got none")
	}
}

func TestWebhookHandler_IgnoresNonSelfHosted(t *testing.T) {
	c := newTestClient()
	secret := "test-secret"
	handler, events := c.WebhookHandler(secret)

	payload := map[string]any{
		"action": "queued",
		"workflow_job": map[string]any{
			"id":     123,
			"labels": []string{"ubuntu-latest"},
		},
		"repository": map[string]any{
			"name": "my-repo",
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", signPayload(body, secret))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case evt := <-events:
		t.Fatalf("expected no event, got %+v", evt)
	default:
		// expected
	}
}

func TestWebhookHandler_IgnoresUntrackedRepo(t *testing.T) {
	c := newTestClient("repo1") // only tracks repo1
	secret := "test-secret"
	handler, events := c.WebhookHandler(secret)

	payload := map[string]any{
		"action": "queued",
		"workflow_job": map[string]any{
			"id":     123,
			"labels": []string{"self-hosted", "linux"},
		},
		"repository": map[string]any{
			"name": "untracked-repo",
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", signPayload(body, secret))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case evt := <-events:
		t.Fatalf("expected no event for untracked repo, got %+v", evt)
	default:
		// expected
	}
}

func TestWebhookHandler_NoSecretSkipsVerification(t *testing.T) {
	c := newTestClient()
	handler, _ := c.WebhookHandler("") // no secret

	payload := map[string]any{
		"action": "queued",
		"workflow_job": map[string]any{
			"id":     1,
			"labels": []string{"self-hosted"},
		},
		"repository": map[string]any{
			"name": "repo",
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	// No signature header
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("no-secret status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestWebhookHandler_MalformedJSON(t *testing.T) {
	c := newTestClient()
	secret := "test-secret"
	handler, _ := c.WebhookHandler(secret)

	body := []byte(`{not json`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", signPayload(body, secret))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestWebhookHandler_NilWorkflowJob(t *testing.T) {
	c := newTestClient()
	secret := "test-secret"
	handler, events := c.WebhookHandler(secret)

	payload := map[string]any{
		"action":     "queued",
		"repository": map[string]any{"name": "repo"},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", signPayload(body, secret))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("nil workflow_job status = %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case evt := <-events:
		t.Fatalf("expected no event for nil workflow_job, got %+v", evt)
	default:
	}
}
