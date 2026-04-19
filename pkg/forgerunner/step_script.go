package forgerunner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ScriptResult is the outcome of running a script step.
type ScriptResult struct {
	ExitCode   int
	Outputs    map[string]string // from GITHUB_OUTPUT file
	EnvChanges map[string]string // from GITHUB_ENV file
	PathAdds   []string          // from GITHUB_PATH file
}

// RunScript executes a run: step by writing the script to a temp file,
// spawning the appropriate shell, and capturing output.
func RunScript(ctx context.Context, step *Step, env map[string]string, workDir string, reporter *LogReporter, masker *SecretMasker) (*ScriptResult, error) {
	if step.Run == "" {
		return &ScriptResult{}, nil
	}

	// Create a temp directory for this step's files.
	tmpDir, err := os.MkdirTemp("", "forge-step-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
			reporter.AddLine(fmt.Sprintf("warning: failed to clean temp dir: %v", rmErr))
		}
	}()

	// Create file-command files.
	outputFile := filepath.Join(tmpDir, "output")
	envFile := filepath.Join(tmpDir, "env")
	pathFile := filepath.Join(tmpDir, "path")
	summaryFile := filepath.Join(tmpDir, "summary")
	stateFile := filepath.Join(tmpDir, "state")

	for _, f := range []string{outputFile, envFile, pathFile, summaryFile, stateFile} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			return nil, fmt.Errorf("create file command file %s: %w", f, err)
		}
	}

	// Determine shell and write script file.
	shell, args, ext := resolveShell(step.Shell)
	scriptFile := filepath.Join(tmpDir, "script"+ext)
	if err := os.WriteFile(scriptFile, []byte(step.Run), 0o755); err != nil {
		return nil, fmt.Errorf("write script: %w", err)
	}

	// Build command.
	cmdArgs := append(args, scriptFile)
	cmd := exec.CommandContext(ctx, shell, cmdArgs...)
	cmd.Dir = workDir

	// Build environment: inherit current env, overlay job/step env, add file command paths.
	cmdEnv := os.Environ()
	for k, v := range env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	for k, v := range step.Env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	cmdEnv = append(cmdEnv,
		"GITHUB_OUTPUT="+outputFile,
		"GITHUB_ENV="+envFile,
		"GITHUB_PATH="+pathFile,
		"GITHUB_STEP_SUMMARY="+summaryFile,
		"GITHUB_STATE="+stateFile,
	)
	cmd.Env = cmdEnv

	// Capture stdout and stderr together.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}

	// Process output line by line.
	processOutput(stdout, reporter, masker)

	// Wait for process to complete.
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("wait: %w", err)
		}
	}

	// Read file-based commands.
	fc, err := ParseFileCommands(outputFile, envFile, pathFile)
	if err != nil {
		reporter.AddLine(fmt.Sprintf("warning: failed to parse file commands: %v", err))
		fc = &FileCommands{
			Outputs: map[string]string{},
			EnvVars: map[string]string{},
		}
	}

	return &ScriptResult{
		ExitCode:   exitCode,
		Outputs:    fc.Outputs,
		EnvChanges: fc.EnvVars,
		PathAdds:   fc.PathAdds,
	}, nil
}

// processOutput reads lines from the process output, parses workflow commands,
// and sends non-command lines to the log reporter.
func processOutput(r io.Reader, reporter *LogReporter, masker *SecretMasker) {
	scanner := bufio.NewScanner(r)
	// Increase buffer size for long lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		cmd, isCmd := ParseCommand(line)
		if !isCmd {
			reporter.AddLine(line)
			continue
		}

		switch cmd.Name {
		case "add-mask":
			if masker != nil {
				masker.AddSecret(cmd.Value)
			}
		case "error", "warning", "notice", "debug":
			// Annotations — pass through as log lines with prefix.
			reporter.AddLine(fmt.Sprintf("[%s] %s", cmd.Name, cmd.Value))
		case "group":
			reporter.AddLine(fmt.Sprintf("##[group]%s", cmd.Value))
		case "endgroup":
			reporter.AddLine("##[endgroup]")
		case "stop-commands":
			// TODO: disable command processing until ::token::
		case "set-output":
			// Legacy command — file-based GITHUB_OUTPUT is preferred.
			// Handled via file commands after step completes.
		case "set-env":
			// Legacy — handled via GITHUB_ENV file.
		case "add-path":
			// Legacy — handled via GITHUB_PATH file.
		default:
			// Unknown command — pass through.
			reporter.AddLine(line)
		}
	}
}

// resolveShell determines the shell binary, arguments, and script extension
// for the given shell specifier. If empty, platform defaults are used.
func resolveShell(shell string) (bin string, args []string, ext string) {
	switch strings.ToLower(shell) {
	case "bash":
		return "bash", []string{"--noprofile", "--norc", "-eo", "pipefail"}, ".sh"
	case "sh":
		return "sh", []string{"-e"}, ".sh"
	case "pwsh", "powershell":
		return shell, []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}, ".ps1"
	case "cmd":
		return "cmd", []string{"/D", "/E:ON", "/V:OFF", "/S", "/C", "call"}, ".cmd"
	case "python":
		return "python", []string{}, ".py"
	case "":
		return defaultShell()
	default:
		// Custom shell — pass script as last argument.
		return shell, []string{}, ".sh"
	}
}
