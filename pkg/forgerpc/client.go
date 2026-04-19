// Package forgerpc implements a lightweight ConnectRPC client for the
// Gitea/Forgejo RunnerService protocol.
//
// Both platforms share the same runner.v1.RunnerService API (Register,
// Declare, FetchTask) with minor response differences (Forgejo returns
// a runner UUID). This package uses raw HTTP + JSON encoding to avoid
// pulling in the full ConnectRPC and protobuf dependency trees.
//
// Wire format: Connect protocol unary RPCs over HTTP/1.1 with JSON.
//
//	POST {instanceURL}/api/actions/runner.v1.RunnerService/{Method}
//	Content-Type: application/json
//	Connect-Protocol-Version: 1
//
// Auth (post-registration):
//
//	x-runner-uuid: {uuid}
//	x-runner-token: {token}
//
// Reference:
//   - Gitea proto:   https://gitea.com/gitea/actions-proto-def
//   - Forgejo proto: https://code.forgejo.org/forgejo/actions-proto
//   - Connect spec:  https://connectrpc.com/docs/protocol
package forgerpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	servicePath = "runner.v1.RunnerService"
	apiPrefix   = "/api/actions"
)

// Client talks to a Gitea or Forgejo instance's RunnerService via ConnectRPC.
type Client struct {
	instanceURL string // e.g., "https://gitea.example.com" (no trailing slash)
	baseURL     string // e.g., "https://gitea.example.com/api/actions"
	httpClient  *http.Client

	// Set after Register — used for auth in subsequent calls.
	uuid  string
	token string
}

// NewClient creates a ConnectRPC client for the given forge instance URL.
// Pass nil for httpClient to use a default with 30s timeout.
func NewClient(instanceURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	instanceURL = strings.TrimRight(instanceURL, "/")
	return &Client{
		instanceURL: instanceURL,
		baseURL:     instanceURL + apiPrefix,
		httpClient:  httpClient,
	}
}

// SetAuth sets the runner credentials for authenticated RPCs.
// Called automatically by Register; only needed when restoring saved credentials.
func (c *Client) SetAuth(uuid, token string) {
	c.uuid = uuid
	c.token = token
}

// UUID returns the runner UUID set after registration.
func (c *Client) UUID() string { return c.uuid }

// Token returns the runner token set after registration.
func (c *Client) Token() string { return c.token }

// ---------- REST API ----------

// Version returns the forge instance version string (e.g. "1.26.0" for Gitea,
// "9.0.3" for Forgejo).
func (c *Client) Version(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.instanceURL+"/api/v1/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("version: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			_ = closeErr
		}
	}()
	var v struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", fmt.Errorf("version: decode: %w", err)
	}
	return v.Version, nil
}

// RegistrationToken fetches a runner registration token from the forge REST API.
//
// Scope is determined by owner/repo:
//   - owner="" repo="" → instance-level (admin)
//   - owner="org" repo="" → org-level
//   - owner="org" repo="repo" → repo-level
//
// The HTTP method for this endpoint diverged across Gitea versions:
//
//	Gitea <1.22:  endpoint does not exist (runners registered via CLI/admin UI)
//	Gitea 1.22–1.23: GET only (POST returns 405)
//	Gitea 1.24–1.25: both GET and POST
//	Gitea 1.26+:     POST only (GET returns 404)
//	Forgejo (all):   GET
//
// This method tries POST first then falls back to GET, covering all versions.
func (c *Client) RegistrationToken(ctx context.Context, apiToken, owner, repo string) (string, error) {
	var path string
	switch {
	case owner != "" && repo != "":
		path = fmt.Sprintf("/api/v1/repos/%s/%s/actions/runners/registration-token", owner, repo)
	case owner != "":
		path = fmt.Sprintf("/api/v1/orgs/%s/actions/runners/registration-token", owner)
	default:
		path = "/api/v1/admin/runners/registration-token"
	}
	url := c.instanceURL + path

	// Try POST first (Gitea 1.26+), fall back to GET (older Gitea, Forgejo).
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "token "+apiToken)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("registration token: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			return "", fmt.Errorf("registration token: close: %w", closeErr)
		}

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			var result struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				return "", fmt.Errorf("registration token: decode: %w", err)
			}
			if result.Token != "" {
				return result.Token, nil
			}
		}
		// 404 or "Runner not found" → try next method
	}

	return "", fmt.Errorf("registration token: neither POST nor GET returned a token at %s", path)
}

// ---------- Types ----------

// Runner holds runner info returned by Register.
type Runner struct {
	ID    int64
	UUID  string
	Name  string
	Token string
}

func (r *Runner) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID    json.RawMessage `json:"id"`
		UUID  string          `json:"uuid"`
		Name  string          `json:"name"`
		Token string          `json:"token"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	id, err := parseFlexInt64(raw.ID)
	if err != nil {
		return fmt.Errorf("parse runner id: %w", err)
	}
	r.ID = id
	r.UUID = raw.UUID
	r.Name = raw.Name
	r.Token = raw.Token
	return nil
}

// Task is a CI task returned by FetchTask.
type Task struct {
	ID              int64
	UUID            string          // Forgejo returns this; Gitea may leave empty
	WorkflowPayload string          // base64-encoded workflow YAML
	Context         json.RawMessage // google.protobuf.Struct — repo/ref/secrets context
}

func (t *Task) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID              json.RawMessage `json:"id"`
		UUID            string          `json:"uuid,omitempty"`
		WorkflowPayload string          `json:"workflowPayload"`
		Context         json.RawMessage `json:"context"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	id, err := parseFlexInt64(raw.ID)
	if err != nil {
		return fmt.Errorf("parse task id: %w", err)
	}
	t.ID = id
	t.UUID = raw.UUID
	t.WorkflowPayload = raw.WorkflowPayload
	t.Context = raw.Context
	return nil
}

// WorkflowYAML decodes the base64 workflow payload to raw YAML bytes.
func (t *Task) WorkflowYAML() ([]byte, error) {
	if t.WorkflowPayload == "" {
		return nil, nil
	}
	// proto3 JSON uses standard base64 with padding; try without padding as fallback.
	data, err := base64.StdEncoding.DecodeString(t.WorkflowPayload)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(t.WorkflowPayload)
		if err != nil {
			return nil, fmt.Errorf("decode workflow payload: %w", err)
		}
	}
	return data, nil
}

// Repo extracts the repository full name (e.g. "owner/repo") from the task context.
// Gitea/Forgejo nest repo info under a "github" key for Actions compatibility.
func (t *Task) Repo() string {
	if len(t.Context) == 0 {
		return ""
	}
	var ctx map[string]json.RawMessage
	if json.Unmarshal(t.Context, &ctx) != nil {
		return ""
	}
	// Primary: context.github.repository (both Gitea and Forgejo use this key)
	if raw, ok := ctx["github"]; ok {
		var gh map[string]any
		if json.Unmarshal(raw, &gh) == nil {
			if repo, ok := gh["repository"].(string); ok {
				return repo
			}
		}
	}
	// Fallback: context.repository (direct field)
	if raw, ok := ctx["repository"]; ok {
		var repo string
		if json.Unmarshal(raw, &repo) == nil {
			return repo
		}
	}
	return ""
}

// EphemerdImage parses the workflow YAML and returns the EPHEMERD_IMAGE
// env var value, if set in any job definition. Returns "" if not found.
func (t *Task) EphemerdImage() string {
	yamlBytes, err := t.WorkflowYAML()
	if err != nil || len(yamlBytes) == 0 {
		return ""
	}
	return parseEphemerdImage(yamlBytes)
}

// TaskResult represents the outcome of a task or step.
type TaskResult int

const (
	ResultUnspecified TaskResult = 0
	ResultSuccess     TaskResult = 1
	ResultFailure     TaskResult = 2
	ResultCancelled   TaskResult = 3
	ResultSkipped     TaskResult = 4
)

// StepState reports the outcome of a single workflow step.
type StepState struct {
	Step      int64      `json:"step"`
	Result    TaskResult `json:"result"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	StoppedAt *time.Time `json:"stoppedAt,omitempty"`
	LogIndex  int64      `json:"logIndex"`
	LogLength int64      `json:"logLength"`
}

// TaskState reports the current state of a task.
type TaskState struct {
	ID        int64       `json:"id"`
	Result    TaskResult  `json:"result"`
	StartedAt *time.Time  `json:"startedAt,omitempty"`
	StoppedAt *time.Time  `json:"stoppedAt,omitempty"`
	Steps     []StepState `json:"steps,omitempty"`
}

// LogRow is a single timestamped log line.
type LogRow struct {
	Time    time.Time `json:"time"`
	Content string    `json:"content"`
}

// UpdateTaskResponse is the response from UpdateTask.
type UpdateTaskResponse struct {
	State       *TaskState `json:"state,omitempty"`
	SentOutputs []string   `json:"sentOutputs,omitempty"`
}

// FetchTaskResult is the response from FetchTask.
type FetchTaskResult struct {
	Task         *Task // nil if no task available
	TasksVersion int64 // pass back to next FetchTask for change detection
}

func (r *FetchTaskResult) UnmarshalJSON(data []byte) error {
	var raw struct {
		Task    *Task           `json:"task"`
		Version json.RawMessage `json:"tasksVersion"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.Task = raw.Task
	if len(raw.Version) > 0 {
		v, err := parseFlexInt64(raw.Version)
		if err != nil {
			return fmt.Errorf("parse tasksVersion: %w", err)
		}
		r.TasksVersion = v
	}
	return nil
}

// RPCError is returned when the ConnectRPC endpoint returns a non-200 status.
type RPCError struct {
	HTTPStatus int
	Code       string // ConnectRPC error code (e.g. "unauthenticated", "not_found")
	Message    string
}

func (e *RPCError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("rpc error: %s: %s (HTTP %d)", e.Code, e.Message, e.HTTPStatus)
	}
	return fmt.Sprintf("rpc error: HTTP %d: %s", e.HTTPStatus, e.Message)
}

// ---------- RPCs ----------

// Register exchanges a registration token for persistent runner credentials.
// On success the client stores the credentials for subsequent authenticated calls.
//
// Labels are plain strings (e.g. "ubuntu-latest:docker://node:20-bookworm").
// The proto field is repeated string, not repeated AgentLabel — use Declare
// for the structured label announcement after registration.
func (c *Client) Register(ctx context.Context, name, regToken, version string, labels []string) (*Runner, error) {
	req := map[string]any{
		"name":    name,
		"token":   regToken,
		"version": version,
		"labels":  labels,
	}

	var resp struct {
		Runner *Runner `json:"runner"`
	}
	if err := c.call(ctx, "Register", req, &resp); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	if resp.Runner == nil {
		return nil, fmt.Errorf("register: empty runner in response")
	}

	// Store credentials for subsequent calls.
	c.uuid = resp.Runner.UUID
	c.token = resp.Runner.Token
	return resp.Runner, nil
}

// Declare announces the runner's labels to the server.
// Call after Register to tell the forge which job labels this runner handles.
//
// Labels should be name-only strings (e.g. "ubuntu-latest"), NOT the full
// docker-mapped format used in Register ("ubuntu-latest:docker://image").
// Forgejo matches runs-on: values against Declare labels, so "ubuntu-latest"
// must match exactly. Use DeclareLabels to convert from Register format.
func (c *Client) Declare(ctx context.Context, labels []string) error {
	req := map[string]any{
		"version": "ephemerd/v1",
		"labels":  labels,
	}
	var resp json.RawMessage
	if err := c.call(ctx, "Declare", req, &resp); err != nil {
		return fmt.Errorf("declare: %w", err)
	}
	return nil
}

// FetchTask polls for an available task. Returns a nil Task if none available.
// The server may long-poll (~5s) before returning an empty response.
//
// Pass tasksVersion from the previous response for efficient change detection;
// use 0 for the first call.
func (c *Client) FetchTask(ctx context.Context, tasksVersion int64) (*FetchTaskResult, error) {
	req := map[string]any{
		"tasksVersion": tasksVersion,
	}
	var result FetchTaskResult
	if err := c.call(ctx, "FetchTask", req, &result); err != nil {
		return nil, fmt.Errorf("fetchTask: %w", err)
	}
	return &result, nil
}

// ---------- Internal ----------

func (c *Client) call(ctx context.Context, method string, reqBody any, respBody any) error {
	url := fmt.Sprintf("%s/%s/%s", c.baseURL, servicePath, method)

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	// Auth headers for post-registration calls.
	if c.uuid != "" {
		req.Header.Set("x-runner-uuid", c.uuid)
	}
	if c.token != "" {
		req.Header.Set("x-runner-token", c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			// Response body already read; close errors are benign.
			_ = closeErr
		}
	}()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		rpcErr := &RPCError{HTTPStatus: resp.StatusCode}
		var errBody struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(respBytes, &errBody) == nil {
			rpcErr.Code = errBody.Code
			rpcErr.Message = errBody.Message
		}
		if rpcErr.Message == "" {
			rpcErr.Message = string(respBytes)
		}
		return rpcErr
	}

	if err := json.Unmarshal(respBytes, respBody); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	return nil
}

// UpdateTask reports the current state of a task (step results, completion).
func (c *Client) UpdateTask(ctx context.Context, state *TaskState, outputs map[string]string) (*UpdateTaskResponse, error) {
	req := map[string]any{
		"state": state,
	}
	if len(outputs) > 0 {
		req["outputs"] = outputs
	}
	var resp UpdateTaskResponse
	if err := c.call(ctx, "UpdateTask", req, &resp); err != nil {
		return nil, fmt.Errorf("updateTask: %w", err)
	}
	return &resp, nil
}

// UpdateLog streams log lines to the forge. Returns the server's acknowledged
// line index. Call with noMore=true after the final flush.
func (c *Client) UpdateLog(ctx context.Context, taskID int64, index int64, rows []LogRow, noMore bool) (int64, error) {
	req := map[string]any{
		"taskId": taskID,
		"index":  index,
		"rows":   rows,
		"noMore": noMore,
	}
	var resp struct {
		AckIndex json.RawMessage `json:"ackIndex"`
	}
	if err := c.call(ctx, "UpdateLog", req, &resp); err != nil {
		return 0, fmt.Errorf("updateLog: %w", err)
	}
	ack, err := parseFlexInt64(resp.AckIndex)
	if err != nil {
		return 0, fmt.Errorf("parse ackIndex: %w", err)
	}
	return ack, nil
}

// ---------- Helpers ----------

// DeclareLabels converts Register-format labels ("ubuntu-latest:docker://image")
// to Declare-format labels ("ubuntu-latest") by stripping the :type://image suffix.
func DeclareLabels(registerLabels []string) []string {
	out := make([]string, len(registerLabels))
	for i, l := range registerLabels {
		if idx := strings.Index(l, ":"); idx > 0 {
			out[i] = l[:idx]
		} else {
			out[i] = l
		}
	}
	return out
}

// parseFlexInt64 handles proto3 JSON int64 encoding where values may appear
// as either JSON numbers (123) or JSON strings ("123").
func parseFlexInt64(data json.RawMessage) (int64, error) {
	if len(data) == 0 || string(data) == "null" {
		return 0, nil
	}
	// JSON number
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		return n, nil
	}
	// JSON string (proto3 JSON int64 encoding)
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return strconv.ParseInt(s, 10, 64)
	}
	return 0, fmt.Errorf("cannot parse %s as int64", string(data))
}

// parseEphemerdImage looks for EPHEMERD_IMAGE in the workflow YAML env.
func parseEphemerdImage(yamlBytes []byte) string {
	var workflow struct {
		Jobs map[string]struct {
			Env map[string]string `yaml:"env"`
		} `yaml:"jobs"`
	}
	if yaml.Unmarshal(yamlBytes, &workflow) != nil {
		return ""
	}
	for _, job := range workflow.Jobs {
		if img, ok := job.Env["EPHEMERD_IMAGE"]; ok && img != "" {
			return img
		}
	}
	return ""
}
