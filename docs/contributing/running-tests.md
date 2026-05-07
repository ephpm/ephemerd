---
title: Running Tests
weight: 3
---

## Unit tests

Run all unit tests with Mage (downloads embedded dependencies first if needed):

```bash
mage test
```

Or run them directly with `go test`:

```bash
go test ./pkg/...
```

## E2E tests

### Unprivileged

The basic e2e suite tests webhook round-trips and requires a `GITHUB_TOKEN`:

```bash
GITHUB_TOKEN=ghp_... mage e2e
```

### Privileged

The privileged e2e tests start containerd and create real containers. They require root:

```bash
sudo go test -tags "e2e,privileged" -v -timeout 5m ./test/e2e/...
```

### Provider-specific

Each supported forge has its own e2e test that boots the forge via docker-compose, runs a full workflow, and tears down. These require Docker with compose support:

| Target | Forge | Notes |
|---|---|---|
| `mage e2egithub` | GitHub | Uses a fake in-process API server -- no token, Docker, or containerd needed |
| `mage e2eforgejo` | Forgejo | Boots Forgejo via docker-compose |
| `mage e2egitea` | Gitea | Boots Gitea via docker-compose |
| `mage e2egitlab` | GitLab CE | Boots GitLab CE (~3GB image, 2-4 min boot, 10m timeout) |
| `mage e2ewoodpecker` | Woodpecker CI | Boots Gitea + Woodpecker Server + Agent via docker-compose |

## Windows VM tests

The `pkg/vm` tests on Windows require a dummy `pkg/vm/embed/ephemerd-linux` file. This is created automatically by `mage download:all` or `mage build`. If running tests without a full build, create the file manually:

```bash
touch pkg/vm/embed/ephemerd-linux
```

## Code style

Before pushing any changes, always run the full CI pipeline:

```bash
mage ci
```

This runs the same lint, test, and build steps as GitHub Actions.

Key conventions:

- **No `_ =` to silence errors.** Always handle errors -- check and log, return, or wrap with context. Add a comment only if there is truly no way to handle it (e.g., deferred `Close` with no logger).
- **No Co-Authored-By lines** in commits.
- **Wrap errors with context:** `fmt.Errorf("operation: %w", err)`.
- **Structured logging:** use `log/slog` throughout.
