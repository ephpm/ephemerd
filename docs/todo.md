# ephemerd TODO

## Phase 1: Core Runtime (get a single job running)

### Embedded containerd server
- [ ] Import containerd server packages (`github.com/containerd/containerd/v2/cmd/containerd/command` or equivalent)
- [ ] Start containerd in-process in a goroutine (k3s/rke2 pattern)
- [ ] Generate containerd config TOML at startup (root dir, state dir, snapshotter)
- [ ] Replace the current client-connect-to-socket approach with in-process server
- [ ] Verify containerd starts and is healthy before accepting jobs
- [ ] On Windows: configure containerd for Windows containers + Hyper-V isolation
- [ ] On Linux: configure overlayfs snapshotter (or native if overlayfs unavailable)
- [ ] Graceful shutdown: stop containerd cleanly when ephemerd exits

### Runner container image
- [ ] Create a Dockerfile for Linux runner image (ubuntu base + GitHub Actions runner binary)
- [ ] Create a Dockerfile for Windows runner image (servercore base + runner binary)
- [ ] Auto-download the latest GitHub Actions runner release on first run or cache miss
- [ ] Image entrypoint: configure runner with JIT config, start `run.sh` / `run.cmd`
- [ ] Publish images to ghcr.io/ephpm/ephemerd-runner (linux/amd64, linux/arm64, windows/amd64)

### GitHub JIT runner flow
- [ ] Verify `GenerateRepoJITConfig` request fields match GitHub's actual API
- [ ] Test the encoded JIT config is passed correctly to the runner binary inside the container
- [ ] Confirm the runner registers, picks up the job, executes, and deregisters
- [ ] Handle runner auto-removal on job completion (GitHub does this for ephemeral runners)
- [ ] Handle case where JIT runner registration fails (rate limits, permissions)

### End-to-end: single job
- [ ] Write a test workflow in a repo that targets `[self-hosted, linux, x64]`
- [ ] Start ephemerd on a Linux host with containerd available
- [ ] Trigger the workflow, verify: webhook received → container created → job runs → container destroyed
- [ ] Verify clean state: no leftover containers/snapshots after job completion

## Phase 2: Production Hardening

### Webhook server
- [ ] TLS support (or document reverse proxy setup with nginx/caddy)
- [ ] Webhook signature verification tested against real GitHub payloads
- [ ] Handle webhook replay / duplicate delivery (idempotency by job ID)
- [ ] Rate limiting on incoming webhooks
- [ ] Health endpoint for monitoring (`/healthz`)

### Scheduler robustness
- [ ] Job timeout enforcement (kill container after configured timeout)
- [ ] Handle container crash/OOM — clean up and report failure
- [ ] Handle ephemerd restart — detect and clean up orphaned containers from previous run
- [ ] Graceful shutdown: wait for running jobs to finish (with timeout) before exiting
- [ ] Metrics: active jobs, total jobs, container startup time, job duration

### Networking
- [ ] Container networking setup for Linux (CNI or containerd default bridge)
- [ ] Container networking for Windows (NAT mode with Hyper-V isolation)
- [ ] Outbound internet access from containers (for package downloads, git clone, etc.)
- [ ] DNS resolution inside containers

### Logging
- [ ] Stream container stdout/stderr to ephemerd logs (debug level)
- [ ] Structured logging with job ID context on all log lines
- [ ] Log rotation or integration with journald/Windows Event Log

## Phase 3: Multi-platform

### Windows support
- [ ] Build and test ephemerd.exe on Windows
- [ ] Containerd in-process on Windows (confirm it works like rke2)
- [ ] Hyper-V isolation verified end-to-end
- [ ] Windows service integration (sc.exe / NSSM for running as a service)
- [ ] Test with a real Windows CI job (e.g. the php-sdk build)

### ARM64
- [ ] Cross-compile for linux/arm64
- [ ] Test on Raspberry Pi or ARM64 server
- [ ] Cross-compile for windows/arm64 (when ecosystem is ready)

## Phase 4: Polish

### CLI
- [ ] `ephemerd status` — show running jobs, containerd health, uptime
- [ ] `ephemerd images` — list cached runner images, pull/prune
- [ ] `ephemerd drain` — stop accepting new jobs, wait for running jobs to finish
- [ ] `ephemerd config check` — validate config file without starting

### Configuration
- [ ] Support GitHub App authentication (app_id + private key) in addition to PAT
- [ ] Per-repo image overrides (e.g. use a PHP-specific image for php-sdk)
- [ ] Per-repo label overrides
- [ ] Environment variable overrides for all config fields (`EPHEMERD_` prefix)
- [ ] Config hot-reload on SIGHUP (repos, labels, concurrency)

### Security
- [ ] Document required permissions for GitHub token (repo, admin:org)
- [ ] Document required host permissions (containerd/Hyper-V access)
- [ ] Restrict container capabilities (no privileged, drop all caps, read-only rootfs where possible)
- [ ] Seccomp profile for Linux containers
- [ ] Network policy: restrict container-to-host access

### CI/CD for ephemerd itself
- [ ] GitHub Actions workflow: build + test on push
- [ ] Cross-platform build matrix (linux/amd64, linux/arm64, windows/amd64)
- [ ] Release workflow: build binaries + publish to GitHub Releases
- [ ] Dogfood: run ephemerd's own CI on ephemerd runners

### Documentation
- [ ] README with quickstart (install binary, create config, start, add webhook)
- [ ] Webhook setup guide (GitHub repo settings → Webhooks)
- [ ] Windows setup guide (enable Hyper-V, install ephemerd as service)
- [ ] Homelab guide (Raspberry Pi, mini PC, etc.)
- [ ] Troubleshooting guide (`ephemerd ctrctl` for debugging)
- [ ] Runner image customization guide (add your own tools)
