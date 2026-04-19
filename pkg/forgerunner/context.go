package forgerunner

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/ephpm/ephemerd/pkg/forgerpc"
)

// BuildEnv extracts environment variables from a forge task payload.
// These mirror the GITHUB_* and RUNNER_* variables that GHA sets.
func BuildEnv(task *forgerpc.Task, runnerName string) map[string]string {
	env := map[string]string{
		// Runner context
		"RUNNER_OS":        runnerOS(),
		"RUNNER_ARCH":      runnerArch(),
		"RUNNER_NAME":      runnerName,
		"RUNNER_TOOL_CACHE": "/opt/hostedtoolcache",
		"RUNNER_TEMP":      os.TempDir(),

		// CI indicators
		"CI":      "true",
		"ACTIONS": "true",
	}

	if len(task.Context) == 0 {
		return env
	}

	// The forge task context is a proto Struct. The top-level keys vary
	// but both Forgejo and Gitea put repo/ref info under "github" for
	// compatibility with Actions workflows.
	var ctx map[string]json.RawMessage
	if json.Unmarshal(task.Context, &ctx) != nil {
		return env
	}

	// Extract github context fields.
	if raw, ok := ctx["github"]; ok {
		var gh map[string]any
		if json.Unmarshal(raw, &gh) == nil {
			setIfStr(env, "GITHUB_REPOSITORY", gh, "repository")
			setIfStr(env, "GITHUB_REF", gh, "ref")
			setIfStr(env, "GITHUB_SHA", gh, "sha")
			setIfStr(env, "GITHUB_REF_NAME", gh, "ref_name")
			setIfStr(env, "GITHUB_REF_TYPE", gh, "ref_type")
			setIfStr(env, "GITHUB_ACTOR", gh, "actor")
			setIfStr(env, "GITHUB_EVENT_NAME", gh, "event_name")
			setIfStr(env, "GITHUB_SERVER_URL", gh, "server_url")
			setIfStr(env, "GITHUB_API_URL", gh, "api_url")
			setIfStr(env, "GITHUB_WORKSPACE", gh, "workspace")
			setIfStr(env, "GITHUB_RUN_ID", gh, "run_id")
			setIfStr(env, "GITHUB_RUN_NUMBER", gh, "run_number")
			setIfStr(env, "GITHUB_WORKFLOW", gh, "workflow")
			setIfStr(env, "GITHUB_JOB", gh, "job")
			setIfStr(env, "GITHUB_TOKEN", gh, "token")

			// Derive owner from repository "owner/repo".
			if repo, ok := gh["repository"].(string); ok {
				if owner, _, ok := strings.Cut(repo, "/"); ok {
					env["GITHUB_REPOSITORY_OWNER"] = owner
				}
			}
		}
	}

	// Extract secrets into environment.
	if raw, ok := ctx["secrets"]; ok {
		var secrets map[string]string
		if json.Unmarshal(raw, &secrets) == nil {
			for k, v := range secrets {
				// Secrets are exposed as env vars with a SECRET_ prefix
				// only if the workflow explicitly maps them. For the
				// GITHUB_TOKEN secret, also set it directly.
				if strings.EqualFold(k, "GITHUB_TOKEN") && v != "" {
					env["GITHUB_TOKEN"] = v
				}
			}
		}
	}

	// Extract vars (repository/org variables).
	if raw, ok := ctx["vars"]; ok {
		var vars map[string]string
		if json.Unmarshal(raw, &vars) == nil {
			for k, v := range vars {
				env["VARS_"+k] = v
			}
		}
	}

	return env
}

// SecretsFromContext extracts secret values from the task context for masking.
func SecretsFromContext(task *forgerpc.Task) []string {
	if len(task.Context) == 0 {
		return nil
	}
	var ctx map[string]json.RawMessage
	if json.Unmarshal(task.Context, &ctx) != nil {
		return nil
	}
	raw, ok := ctx["secrets"]
	if !ok {
		return nil
	}
	var secrets map[string]string
	if json.Unmarshal(raw, &secrets) != nil {
		return nil
	}
	var values []string
	for _, v := range secrets {
		if v != "" {
			values = append(values, v)
		}
	}
	return values
}

func setIfStr(env map[string]string, key string, m map[string]any, field string) {
	switch v := m[field].(type) {
	case string:
		if v != "" {
			env[key] = v
		}
	case float64:
		// JSON numbers (run_id, run_number) come as float64.
		env[key] = fmt.Sprintf("%g", v)
	}
}

func runnerOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

func runnerArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "X64"
	case "arm64":
		return "ARM64"
	default:
		return runtime.GOARCH
	}
}
