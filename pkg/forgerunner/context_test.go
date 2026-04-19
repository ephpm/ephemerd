package forgerunner

import (
	"encoding/json"
	"testing"

	"github.com/ephpm/ephemerd/pkg/forgerpc"
)

func TestBuildEnv(t *testing.T) {
	ctx := `{
		"github": {
			"repository": "myorg/myrepo",
			"ref": "refs/heads/main",
			"sha": "abc123",
			"actor": "user1",
			"event_name": "push",
			"server_url": "https://codeberg.org",
			"run_id": 42,
			"run_number": 7
		},
		"secrets": {
			"GITHUB_TOKEN": "ghs_xxx",
			"MY_SECRET": "secret-val"
		},
		"vars": {
			"APP_ENV": "staging"
		}
	}`

	task := &forgerpc.Task{
		Context: json.RawMessage(ctx),
	}

	env := BuildEnv(task, "test-runner")

	checks := map[string]string{
		"GITHUB_REPOSITORY":       "myorg/myrepo",
		"GITHUB_REPOSITORY_OWNER": "myorg",
		"GITHUB_REF":              "refs/heads/main",
		"GITHUB_SHA":              "abc123",
		"GITHUB_ACTOR":            "user1",
		"GITHUB_EVENT_NAME":       "push",
		"GITHUB_SERVER_URL":       "https://codeberg.org",
		"GITHUB_TOKEN":            "ghs_xxx",
		"RUNNER_NAME":             "test-runner",
		"CI":                      "true",
		"VARS_APP_ENV":            "staging",
	}

	for k, want := range checks {
		if got := env[k]; got != want {
			t.Errorf("env[%s] = %q, want %q", k, got, want)
		}
	}

	// run_id comes as float64 from JSON, should be formatted as "42".
	if got := env["GITHUB_RUN_ID"]; got != "42" {
		t.Errorf("env[GITHUB_RUN_ID] = %q, want 42", got)
	}
}

func TestBuildEnv_EmptyContext(t *testing.T) {
	task := &forgerpc.Task{}
	env := BuildEnv(task, "runner")

	if env["CI"] != "true" {
		t.Error("CI should always be set")
	}
	if env["RUNNER_NAME"] != "runner" {
		t.Errorf("RUNNER_NAME = %q, want runner", env["RUNNER_NAME"])
	}
}

func TestSecretsFromContext(t *testing.T) {
	ctx := `{"secrets":{"TOKEN":"abc","EMPTY":"","KEY":"xyz"}}`
	task := &forgerpc.Task{Context: json.RawMessage(ctx)}

	secrets := SecretsFromContext(task)
	if len(secrets) != 2 {
		t.Errorf("got %d secrets, want 2 (empty string filtered)", len(secrets))
	}
}

func TestSecretsFromContext_NoSecrets(t *testing.T) {
	task := &forgerpc.Task{Context: json.RawMessage(`{}`)}
	secrets := SecretsFromContext(task)
	if len(secrets) != 0 {
		t.Errorf("got %d secrets, want 0", len(secrets))
	}
}
