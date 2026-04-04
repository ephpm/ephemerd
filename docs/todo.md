# ephemerd TODO

## Phase 1: Core Runtime (get a single job running)

### Embedded containerd server
- [x] Import containerd server packages (`ctdserver` and `srvconfig`)
- [x] Start containerd in-process via `ctdserver.New()` + `ServeGRPC()`/`ServeTTRPC()` in goroutines
- [x] Build server config programmatically (root dir, state dir, socket path)
- [x] Connect client to in-process server via socket with health check retry loop
- [x] Verify containerd starts and is healthy before accepting jobs
- [x] Graceful shutdown: `srv.Stop()` cleans up in-process server

### Embedded runner binary
- [x] Embed GitHub Actions runner tarball via `go:embed` (downloaded at build time)
- [x] Extract to `<dataDir>/runners/<version>/` on startup (cached after first run)
- [x] Bind-mount runner directory read-only into job containers at `/actions-runner`
- [x] Override container entrypoint to `run.sh --jitconfig <token>`
- [x] Makefile `download-runner` target fetches correct platform tarball
- [x] Version injected via ldflags at build time

### GitHub JIT runner flow
- [x] Fix double base64 encoding of JIT config
- [x] Wire webhook port and secret from config to scheduler
- [x] Track runner ID for cleanup on failure
- [x] Add backoff on JIT registration failure
- [x] Verify `GenerateRepoJITConfig` request fields match GitHub's actual API
- [x] Auto-pull container image if not present locally
- [x] Remove ghost runner from GitHub if container fails to start after JIT registration
- [ ] Confirm the runner registers, picks up the job, executes, and deregisters (manual test)

### End-to-end: single job
- [x] Write a test workflow in a repo that targets `[self-hosted, linux, x64]`
- [ ] Start ephemerd on a Linux host, trigger workflow, verify full lifecycle
- [ ] Verify clean state: no leftover containers/snapshots after job completion

## Phase 2: Production Hardening

### Job discovery
- [x] Polling mode (default): check GitHub API every 10s, zero config needed
- [x] Webhook mode: TLS-enabled HTTP server for instant GitHub push events
- [x] Both modes feed unified event channel with dedup

### Scheduler robustness
- [x] Job timeout enforcement (context.WithTimeout from parsed config duration)
- [x] Handle ephemerd restart — detect and clean up orphaned containers
- [x] Graceful shutdown: drain mode, wait with configurable timeout, then force-kill
- [x] Webhook idempotency: dedup by job ID with TTL map
- [x] Health endpoint (`/healthz` returns JSON: active jobs, uptime, draining)
- [x] Handle container crash/OOM — detect exit 137, log appropriately, always clean up
- [ ] Metrics: active jobs, total jobs, container startup time, job duration

### Networking
- [x] Container networking for Linux (CNI bridge with NAT via go-cni)
- [x] Firewall rules: deny RFC 1918 + link-local ranges to isolate homelab
- [x] Outbound internet access from containers (ipMasq on bridge)
- [x] DNS resolution inside containers (mount /etc/resolv.conf read-only)

### Logging
- [x] Per-job container log capture to `<dataDir>/logs/<id>.log`
- [ ] Structured logging with job ID context through runtime calls
- [ ] Log rotation or integration with journald/Windows Event Log

## Phase 3: Windows Support

- [ ] HNS NAT networking (Windows equivalent of CNI bridge)
- [ ] Windows Firewall rules for LAN isolation
- [ ] Verify containerd in-process on Windows
- [ ] Hyper-V isolation verified end-to-end
- [ ] Windows service integration (sc.exe / NSSM)
- [ ] Test with a real Windows CI job on ephpm org runner

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
- [ ] Restrict container capabilities (no privileged, drop all caps, read-only rootfs where possible)
- [ ] Seccomp profile for Linux containers
- [ ] Network policy: restrict container-to-host access

### CI/CD
- [ ] Goreleaser config for cross-platform builds (linux/amd64, linux/arm64, windows/amd64)
- [ ] GitHub Actions workflow: build + test on push
- [ ] Release workflow: build binaries + publish to GitHub Releases
- [ ] Dogfood: run ephemerd's own CI on ephemerd runners

### Documentation
- [ ] README with quickstart (install binary, create config, start)
- [ ] Configuration reference
- [ ] Windows setup guide (enable Hyper-V, install as service)
- [ ] Homelab guide (Raspberry Pi, mini PC, etc.)
- [ ] Troubleshooting guide (`ephemerd ctrctl` for debugging)
- [ ] Runner image customization guide
