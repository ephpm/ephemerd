# ephemerd run

Run a GitHub Actions workflow locally without pushing to GitHub. Executes workflow steps in an isolated container on your machine.

## Usage

```
ephemerd run [--workflow <path>] [--job <name>]
```

## Flags

- `--workflow` — path to the workflow YAML file (default: auto-detect from `.github/workflows/`)
- `--job` — run only a specific job from the workflow (default: run all jobs)

## What it does

1. Parses the workflow YAML file
2. Creates an isolated container with the same image ephemerd uses for CI
3. Bind-mounts the current repository into the container
4. Executes each `run:` step in sequence
5. Destroys the container when done

## Limitations

- `uses:` steps (GitHub Actions) are not supported — only `run:` steps execute
- `services:` blocks are not supported
- Environment secrets from GitHub are not available
- Matrix strategies are not expanded

## When to use it

- Quick local testing of shell-based CI steps before pushing
- Debugging failing CI in an environment identical to what ephemerd provisions
- Running build scripts in isolation without Docker
