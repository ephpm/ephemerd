# ephemerd TODO

## Phase 1: Core Runtime (get a single job running)

### Embedded containerd server
- [x] Import containerd server packages (`ctdserver` and `srvconfig`)
- [x] Start containerd in-process via `ctdserver.New()` + `ServeGRPC()`/`ServeTTRPC()` in goroutines
- [x] Build server config programmatically (root dir, state dir, socket path)
- [x] Connect client to in-process server via socket with health check retry loop
- [x] Verify containerd starts and is healthy before accepting jobs
- [x] Graceful shutdown: `srv.Stop()` cleans up in-process server
- [ ] On Windows: verify in-process containerd works with Hyper-V isolation
- [ ] On Linux: verify overlayfs snapshotter is selected by default

### Embedded runner binary
- [x] Embed GitHub Actions runner tarball via `go:embed` (downloaded at build time)
- [x] Extract to `<dataDir>/runners/<version>/` on startup (cached after first run)
- [x] Bind-mount runner directory read-only into job containers at `/actions-runner`
- [x] Override container entrypoint to `run.sh --jitconfig <token>`
- [x] Makefile `download-runner` target fetches correct platform tarball
- [x] Version injected via ldflags at build time

### GitHub JIT runner flow
- [x] Fix double base64 encoding of JIT config (was wrapping already-encoded value)
- [x] Wire webhook port and secret from config to scheduler
- [x] Track runner ID for cleanup on failure
- [x] Add backoff on JIT registration failure (rate limits, permissions)
- [ ] Verify `GenerateRepoJITConfig` request fields match GitHub's actual API
- [ ] Auto-pull container image if not present locally
- [ ] Remove ghost runner from GitHub if container fails to start after JIT registration
- [ ] Confirm the runner registers, picks up the job, executes, and deregisters

### End-to-end: single job
- [x] Write a test workflow in a repo that targets `[self-hosted, linux, x64]`
- [ ] Start ephemerd on a Linux host, trigger workflow, verify full lifecycle
- [ ] Verify clean state: no leftover containers/snapshots after job completion

## Phase 2: Production Hardening

### Webhook server
- [ ] TLS support (or document reverse proxy setup with nginx/caddy)
- [ ] Webhook signature verification tested against real GitHub payloads
- [x] Handle webhook replay / duplicate delivery (idempotency by job ID with TTL)
- [ ] Rate limiting on incoming webhooks
- [x] Health endpoint for monitoring (`/healthz` returns JSON: active jobs, uptime, draining status)

### Scheduler robustness
- [x] Job timeout enforcement (context.WithTimeout from parsed config duration)
- [ ] Handle container crash/OOM — clean up and report failure
- [x] Handle ephemerd restart — detect and clean up orphaned containers from previous run
- [x] Graceful shutdown: drain mode, wait for running jobs with configurable timeout, then force-kill
- [ ] Metrics: active jobs, total jobs, container startup time, job duration

### Networking
- [x] Container networking setup for Linux (CNI bridge with NAT via go-cni)
- [x] Firewall rules: deny RFC 1918 + link-local ranges to isolate homelab
- [x] Outbound internet access from containers (ipMasq on bridge)
- [ ] Container networking for Windows (NAT mode with Hyper-V isolation)
- [ ] DNS resolution inside containers (mount resolv.conf)

### Logging
- [x] Per-job container log capture to `<dataDir>/logs/<id>.log`
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
