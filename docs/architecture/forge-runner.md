---
title: Ephemerd Runners
weight: 11
---

`ephemerd-runner-forgejo` and `ephemerd-runner-gitea` are lightweight Go binaries that replace the upstream `forgejo-runner` and `act_runner` for Forgejo and Gitea CI. They use the same ConnectRPC protocol but execute workflow steps directly via `os/exec` — no Docker dependency, no two-container model.

## Why

The upstream runners (forgejo-runner, act_runner) embed nektos/act, which creates a separate Docker container for each job. This two-container model requires a Docker socket (real or fake) and only supports Linux. The ephemerd runners eliminate both limitations.

```
upstream:                  [ runner daemon ] --Docker API--> [ job container ]
ephemerd-runner-forgejo:   [ single container with runner + steps ]
```

## How it works

1. **Register** — exchanges a registration token for persistent runner credentials via ConnectRPC
2. **Declare** — announces the runner's labels to the server
3. **Poll** — long-polls `FetchTask` for available tasks (the server returns raw workflow YAML)
4. **Execute** — parses the workflow, iterates steps, spawns each as a shell process
5. **Report** — streams timestamped log lines via `UpdateLog`, reports step results via `UpdateTask`
6. **Exit** — after one job, the process exits. ephemerd destroys the container and creates a replacement.

## ConnectRPC client

Both runners implement the `runner.v1.RunnerService` ConnectRPC protocol using raw HTTP + JSON — no protobuf or ConnectRPC library dependencies. The wire format is:

```
POST {instanceURL}/api/actions/runner.v1.RunnerService/{Method}
Content-Type: application/json
Connect-Protocol-Version: 1
```

RPCs implemented:
- **Register** — exchange registration token for runner UUID + token
- **Declare** — announce labels
- **FetchTask** — long-poll for tasks (passes `tasksVersion` for efficient change detection)
- **UpdateTask** — report step results and job completion
- **UpdateLog** — stream timestamped, secret-masked log lines

Post-registration auth uses `x-runner-uuid` and `x-runner-token` headers.

## Step execution

Each `run:` step is executed as a shell process:

1. Write the step's script to a temp file
2. Resolve the shell (explicit `shell:` key, or platform default)
3. Set up environment: CI vars, secrets, step outputs from previous steps
4. Create temp files for `GITHUB_OUTPUT`, `GITHUB_ENV`, `GITHUB_PATH`, `GITHUB_STEP_SUMMARY`
5. Spawn the process, capture stdout/stderr line-by-line
6. Parse workflow commands from output (`::error::`, `::warning::`, `::add-mask::`, etc.)
7. Parse the output files for env/path/output changes to carry forward

### Shell resolution

| Platform | Default | Fallbacks |
|----------|---------|-----------|
| Linux/macOS | `bash` | `sh` |
| Windows | `pwsh` | `powershell`, `cmd` |

Custom shells (`python`, etc.) are also supported via the `shell:` step key.

### Workflow commands

The runners handle both inline commands from stdout and file-based commands:

**Inline** (parsed from step output):
- `::error::`, `::warning::`, `::notice::` — annotations
- `::add-mask::` — dynamic secret masking
- `::group::` / `::endgroup::` — log grouping
- `::set-output::` — step outputs (legacy)

**File-based** (via env var temp files):
- `GITHUB_OUTPUT` — step outputs (`key=value` or heredoc `key<<DELIMITER`)
- `GITHUB_ENV` — environment changes carried to subsequent steps
- `GITHUB_PATH` — PATH additions carried to subsequent steps
- `GITHUB_STEP_SUMMARY` — job summary markdown

## Limitations

- **`uses:` steps are skipped** — only `run:` steps execute. Actions (JavaScript, composite, Docker) are detected and logged as warnings. This covers the majority of build/test workflows.
- **Matrix strategies are not expanded** — the server sends one task per matrix combination, so the runner doesn't need to expand them.
- **Service containers not supported** — use `docker run` in step scripts if needed.

## Two binaries

There are two entry points that share the same `pkg/forgerunner` package:

| Binary | Entry point | Env vars | Use case |
|--------|-------------|----------|----------|
| `ephemerd-runner-forgejo` | `cmd/ephemerd-runner-forgejo/main.go` | `FORGEJO_*` | Forgejo instances |
| `ephemerd-runner-gitea` | `cmd/ephemerd-runner-gitea/main.go` | `GITEA_*` | Gitea instances |

The only differences are CLI flag names and the `Platform` string sent during registration. All execution logic is shared.

### CLI flags

| Flag | Env var (Forgejo) | Env var (Gitea) | Description |
|------|-------------------|-----------------|-------------|
| `--instance` | `FORGEJO_INSTANCE_URL` | `GITEA_INSTANCE_URL` | Instance URL |
| `--token` | `FORGEJO_REG_TOKEN` | `GITEA_REG_TOKEN` | Registration token |
| `--name` | `FORGEJO_RUNNER_NAME` | `GITEA_RUNNER_NAME` | Runner display name (default: hostname) |
| `--label` | `FORGEJO_RUNNER_LABELS` | `GITEA_RUNNER_LABELS` | `runs-on` labels (repeatable, default: `ubuntu-latest`) |

## Secret masking

All log output is filtered through a `SecretMasker` that replaces registered secret values with `***`. Secrets shorter than 3 characters are ignored to avoid false positives. Dynamic masking via `::add-mask::` in step output is also supported.

## Package layout

```
cmd/
  ephemerd-runner-forgejo/       Forgejo runner CLI
  ephemerd-runner-gitea/       Gitea runner CLI
pkg/
  forgerunner/
    runner.go         Registration, poll loop, backoff
    executor.go       Job execution engine
    model.go          Workflow/job/step YAML models
    context.go        Task context → environment builder
    commands.go       Workflow command parser + secret masker
    step_script.go    Script step execution
    step_script_unix.go    Unix shell defaults
    step_script_windows.go Windows shell defaults
    log.go            Log batching and streaming
  forgerpc/
    client.go         ConnectRPC client (raw HTTP + JSON)
```

## Comparison

| Aspect | forgejo-runner (act) | ephemerd-runner-forgejo | GHA runner |
|--------|---------------------|-------------|-----------|
| Container model | Two (runner + job) | One | One |
| Docker required | Yes | No | No |
| Execution | act → Docker exec | Direct os/exec | Direct process spawn |
| Multi-OS | Linux only | Linux, Windows, macOS | Linux, Windows, macOS |
| Protocol | ConnectRPC (protobuf) | ConnectRPC (raw HTTP + JSON) | GitHub REST |
| `uses:` actions | Full support | Not yet | Full support |
| Binary size | ~50MB | ~15MB | ~100MB |
