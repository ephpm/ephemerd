---
title: run
weight: 2
---

Run a GitHub Actions workflow locally in an ephemeral container. This is useful for testing workflows without pushing to GitHub.

```
ephemerd run [workflow-file] [flags]
```

## Arguments

The workflow file is a **positional argument**, not a flag. If omitted, ephemerd searches the current directory's `.github/workflows/` for a workflow file.

## Flags

| Flag | Description |
|------|-------------|
| `--job`, `-j` | Run a specific job by name. If omitted, runs the first job in the workflow. |

## Behavior

1. **Locate workflow** -- if no file is specified, calls `workflow.FindWorkflow()` to auto-detect a workflow in `.github/workflows/`.
2. **Parse workflow** -- reads the YAML workflow file and extracts job definitions.
3. **Select job** -- uses `--job` to pick a specific job, or defaults to the first job found.
4. **Detect platform** -- inspects the job's `runs-on` labels to determine the target OS (linux, windows, or macos).
5. **Execute** -- runs the job in a local container using the container runtime.

## WSL delegation on Windows

When running on a Windows host and the workflow targets Linux (based on `runs-on` labels), ephemerd automatically delegates execution to WSL. It:

1. Creates a temporary WSL distro (`vm.NewRunDistro`).
2. Translates the workflow path to an absolute path.
3. Runs the Linux ephemerd binary inside WSL with the same workflow and job arguments.
4. Destroys the WSL distro when the job completes.

## Limitations

The local runner is a simplified execution environment. It does not support:

- **`uses:` steps** -- action references are not resolved or downloaded.
- **`services:`** -- service containers are not started.
- **`secrets`** -- GitHub secrets are not available.
- **`matrix`** -- matrix strategies are not expanded.

## Examples

```bash
# Run the default workflow in the current repo
ephemerd run

# Run a specific workflow file
ephemerd run .github/workflows/ci.yml

# Run a specific job from a workflow
ephemerd run .github/workflows/ci.yml --job build

# Short flag form
ephemerd run .github/workflows/ci.yml -j test
```
