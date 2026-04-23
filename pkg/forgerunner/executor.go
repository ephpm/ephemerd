package forgerunner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ephpm/ephemerd/pkg/forgerpc"
)

// Executor runs a single forge task: parses the workflow, executes steps,
// and reports results back to the forge.
type Executor struct {
	client *forgerpc.Client
	task   *forgerpc.Task
	log    *slog.Logger
}

// NewExecutor creates an executor for a task.
func NewExecutor(client *forgerpc.Client, task *forgerpc.Task, log *slog.Logger) *Executor {
	return &Executor{client: client, task: task, log: log}
}

// Run executes the task end-to-end.
func (e *Executor) Run(ctx context.Context) error {
	// Decode workflow YAML.
	yamlBytes, err := e.task.WorkflowYAML()
	if err != nil {
		return fmt.Errorf("decode workflow: %w", err)
	}

	wf, err := ParseWorkflow(yamlBytes)
	if err != nil {
		return fmt.Errorf("parse workflow: %w", err)
	}

	// Pick the first job. The forge sends one task per job, so there's
	// typically one job in the payload. If multiple exist, run the first.
	var jobName string
	var job *Job
	for k, v := range wf.Jobs {
		jobName = k
		job = v
		break
	}
	if job == nil {
		return fmt.Errorf("no jobs in workflow")
	}

	e.log.Info("executing job",
		"job", jobName,
		"steps", len(job.Steps),
		"runs_on", strings.Join(job.RunsOn.Labels, ","),
	)

	// Build environment from task context.
	hostname, _ := os.Hostname()
	baseEnv := BuildEnv(e.task, hostname)
	baseEnv["GITHUB_JOB"] = jobName

	// Apply job-level env.
	for k, v := range job.Env {
		baseEnv[k] = v
	}

	// Set up secret masking.
	secrets := SecretsFromContext(e.task)
	masker := NewSecretMasker(secrets)

	// Set up log reporter.
	reporter := NewLogReporter(e.client, e.task.ID, masker)

	// Report task started.
	jobStart := time.Now()
	stepStates := make([]forgerpc.StepState, len(job.Steps))

	if _, err := e.client.UpdateTask(ctx, &forgerpc.TaskState{
		ID:        e.task.ID,
		Result:    forgerpc.ResultUnspecified,
		StartedAt: &jobStart,
		Steps:     stepStates,
	}, nil); err != nil {
		e.log.Warn("failed to report task started", "error", err)
	}

	// Execute steps.
	workDir, err := os.Getwd()
	if err != nil {
		workDir = os.TempDir()
	}

	// Workspace defaults to GITHUB_WORKSPACE if set.
	if ws := baseEnv["GITHUB_WORKSPACE"]; ws != "" {
		if info, err := os.Stat(ws); err == nil && info.IsDir() {
			workDir = ws
		}
	}

	jobResult := forgerpc.ResultSuccess
	stepOutputs := map[string]map[string]string{} // step ID → outputs

	for i, step := range job.Steps {
		if ctx.Err() != nil {
			jobResult = forgerpc.ResultCancelled
			break
		}

		// Skip uses: steps for now (action support is a future phase).
		if step.Uses != "" {
			reporter.AddLine(fmt.Sprintf("::warning::Skipping action step: %s (not yet supported)", step.Uses))
			stepStates[i].Step = int64(i)
			stepStates[i].Result = forgerpc.ResultSkipped
			continue
		}

		stepName := step.DisplayName(i)
		reporter.AddLine(fmt.Sprintf("##[group]Run %s", stepName))

		stepStart := time.Now()
		stepStates[i].Step = int64(i)
		stepStates[i].StartedAt = &stepStart
		stepLogStart := reporter.LineCount()

		// Build step env (inherit base + previous step env changes).
		stepEnv := copyEnv(baseEnv)

		// Apply step working directory.
		stepWorkDir := workDir
		if step.WorkingDirectory != "" {
			stepWorkDir = step.WorkingDirectory
		}

		// Run the script.
		result, err := RunScript(ctx, step, stepEnv, stepWorkDir, reporter, masker)

		stepStop := time.Now()
		stepStates[i].StoppedAt = &stepStop
		stepStates[i].LogIndex = stepLogStart
		stepStates[i].LogLength = reporter.LineCount() - stepLogStart

		if err != nil {
			reporter.AddLine(fmt.Sprintf("::error::Step failed: %v", err))
			stepStates[i].Result = forgerpc.ResultFailure
			jobResult = forgerpc.ResultFailure
			reporter.AddLine("##[endgroup]")

			// Flush logs after each step.
			if flushErr := reporter.Flush(ctx); flushErr != nil {
				e.log.Warn("flush logs failed", "error", flushErr)
			}
			break
		}

		if result.ExitCode != 0 {
			reporter.AddLine(fmt.Sprintf("::error::Process exited with code %d", result.ExitCode))
			stepStates[i].Result = forgerpc.ResultFailure
			jobResult = forgerpc.ResultFailure
			reporter.AddLine("##[endgroup]")

			if flushErr := reporter.Flush(ctx); flushErr != nil {
				e.log.Warn("flush logs failed", "error", flushErr)
			}
			break
		}

		stepStates[i].Result = forgerpc.ResultSuccess
		reporter.AddLine("##[endgroup]")

		// Apply env changes and path additions to base env for subsequent steps.
		for k, v := range result.EnvChanges {
			baseEnv[k] = v
		}
		if len(result.PathAdds) > 0 {
			existing := baseEnv["PATH"]
			baseEnv["PATH"] = strings.Join(result.PathAdds, string(os.PathListSeparator)) +
				string(os.PathListSeparator) + existing
		}

		// Store step outputs.
		if step.ID != "" && len(result.Outputs) > 0 {
			stepOutputs[step.ID] = result.Outputs
		}

		// Flush logs after each step.
		if flushErr := reporter.Flush(ctx); flushErr != nil {
			e.log.Warn("flush logs failed", "error", flushErr)
		}
	}

	// Report job completion.
	jobStop := time.Now()
	if _, err := e.client.UpdateTask(ctx, &forgerpc.TaskState{
		ID:        e.task.ID,
		Result:    jobResult,
		StartedAt: &jobStart,
		StoppedAt: &jobStop,
		Steps:     stepStates,
	}, nil); err != nil {
		e.log.Warn("failed to report task completion", "error", err)
	}

	// Close log reporter (final flush + noMore).
	if err := reporter.Close(ctx); err != nil {
		e.log.Warn("failed to close log reporter", "error", err)
	}

	e.log.Info("job completed",
		"job", jobName,
		"result", jobResult,
		"duration", jobStop.Sub(jobStart).Round(time.Millisecond),
	)

	if jobResult != forgerpc.ResultSuccess {
		return fmt.Errorf("job %s failed", jobName)
	}
	return nil
}

func copyEnv(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
