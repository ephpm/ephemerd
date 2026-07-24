package github

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func policyClient(repos, allowedRepos, requiredLabels []string) *Client {
	return &Client{
		cfg: Config{
			Owner:          "testorg",
			Repos:          repos,
			AllowedRepos:   allowedRepos,
			RequiredLabels: requiredLabels,
			Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
}

// TestDispatchAllowed_DefaultAllowsEverything confirms the opt-in nature: an
// empty policy must not change existing behavior.
func TestDispatchAllowed_DefaultAllowsEverything(t *testing.T) {
	c := policyClient(nil, nil, nil)
	if !c.dispatchAllowed("any-repo", []string{"self-hosted"}) {
		t.Fatal("empty policy should allow all dispatch")
	}
}

func TestDispatchAllowed_RepoAllowlist(t *testing.T) {
	c := policyClient(nil, []string{"allowed-repo"}, nil)
	if !c.dispatchAllowed("allowed-repo", []string{"self-hosted"}) {
		t.Error("listed repo should be allowed")
	}
	if c.dispatchAllowed("other-repo", []string{"self-hosted"}) {
		t.Error("unlisted repo should be blocked")
	}
}

func TestDispatchAllowed_RequiredLabels(t *testing.T) {
	c := policyClient(nil, nil, []string{"ephemerd"})
	if !c.dispatchAllowed("repo", []string{"self-hosted", "ephemerd"}) {
		t.Error("job with required label should be allowed")
	}
	if c.dispatchAllowed("repo", []string{"self-hosted", "linux"}) {
		t.Error("job missing required label should be blocked")
	}
}

func TestDispatchAllowed_RepoAndLabelBothRequired(t *testing.T) {
	c := policyClient(nil, []string{"allowed-repo"}, []string{"ephemerd"})
	if !c.dispatchAllowed("allowed-repo", []string{"ephemerd"}) {
		t.Error("both constraints satisfied should allow")
	}
	if c.dispatchAllowed("allowed-repo", []string{"linux"}) {
		t.Error("repo ok but label missing should block")
	}
	if c.dispatchAllowed("other-repo", []string{"ephemerd"}) {
		t.Error("label ok but repo unlisted should block")
	}
}

// TestWebhookHandler_BlockedByPolicyEmitsNoEvent verifies the end-to-end
// handler path drops a signed, tracked, self-hosted job when the dispatch
// policy excludes it, and returns 200 (an intentional drop, not an error).
func TestWebhookHandler_BlockedByPolicyEmitsNoEvent(t *testing.T) {
	c := policyClient([]string{"my-repo"}, nil, []string{"ephemerd"})
	secret := "test-secret"
	handler, events := c.WebhookHandler(secret)

	payload := map[string]any{
		"action": "queued",
		"workflow_job": map[string]any{
			"id":     7,
			"labels": []string{"self-hosted", "linux"}, // no "ephemerd" label
		},
		"repository": map[string]any{"name": "my-repo"},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", signPayload(body, secret))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("blocked-by-policy status = %d, want %d", w.Code, http.StatusOK)
	}
	select {
	case evt := <-events:
		t.Fatalf("expected no event when blocked by policy, got %+v", evt)
	default:
	}
}

// TestWebhookHandler_AllowedByPolicyEmitsEvent is the positive counterpart:
// a job carrying the required label dispatches normally.
func TestWebhookHandler_AllowedByPolicyEmitsEvent(t *testing.T) {
	c := policyClient([]string{"my-repo"}, nil, []string{"ephemerd"})
	secret := "test-secret"
	handler, events := c.WebhookHandler(secret)

	payload := map[string]any{
		"action": "queued",
		"workflow_job": map[string]any{
			"id":     8,
			"labels": []string{"self-hosted", "ephemerd"},
		},
		"repository": map[string]any{"name": "my-repo"},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", signPayload(body, secret))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("allowed-by-policy status = %d, want %d", w.Code, http.StatusOK)
	}
	select {
	case evt := <-events:
		if evt.Repo != "my-repo" {
			t.Errorf("event repo = %q, want my-repo", evt.Repo)
		}
	default:
		t.Fatal("expected event when policy allows, got none")
	}
}
