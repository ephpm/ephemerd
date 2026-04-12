# ephemerd TODOs

## Must Do (Before First Real Use)

### Manual end-to-end verification
- [ ] Start ephemerd on a Linux host, trigger a workflow, verify full lifecycle: webhook/poll → JIT registration → container created → job runs → container destroyed → no orphans
- [ ] Verify on Windows: in-process containerd + Hyper-V isolation end-to-end with a real job
- [ ] Verify on macOS: Virtualization.framework Linux VM boots, containerd starts inside, containers run jobs
- [ ] Verify macOS VM jobs: APFS clone-on-write boot, runner picks up job, VM destroyed after
- [ ] Verify OCI artifact extraction: EPHEMERD_IMAGE pulls image, layers unpacked, files available in macOS VM via virtio-fs

### Windows Linux VM
- [ ] Complete and verify `linuxvm_windows.go` — Hyper-V Linux VM for cross-OS Linux jobs on Windows hosts
- [ ] Document how to create the Linux VHDX image with containerd pre-installed

## Should Do (Production Hardening)

### Windows service integration
- [ ] Install/uninstall as Windows service via sc.exe or NSSM
- [ ] Document Windows setup (enable Hyper-V, install ephemerd, configure service)

### Metrics
- [ ] Prometheus/OpenMetrics endpoint (active jobs, total completed, container startup latency, job duration)
- [ ] Currently only basic counters in `/healthz` JSON response

### Log management
- [ ] Log rotation for per-job logs in `<dataDir>/logs/` — files accumulate with no cleanup
- [ ] Optional journald integration on Linux
- [ ] Optional Windows Event Log integration

### JIT registration robustness
- [ ] Handle GitHub API rate limits gracefully (429 responses)

## Nice to Have (Future)

### Docker-in-Docker (dind) shim
- [ ] Fake Docker Engine API server on Unix socket in each container (architecture in `docs/arch/dind.md`)
- [ ] Container create/start/stop/delete (sidecars via containerd)
- [ ] Image build via embedded buildah
- [ ] `/etc/hosts` injection for service discovery
- [ ] `services:` YAML support via the runner's Docker API calls

### GitLab CI integration
- [ ] Architecture designed in `docs/arch/gitlab.md` — custom executor model
- [ ] Embed `gitlab-runner` binary
- [ ] Generate custom executor config (prepare/run/cleanup scripts)
- [ ] Runner registration with GitLab instance
- [ ] No code exists yet — fully greenfield

### Gitea/Forgejo Actions
- [ ] Near-free since runner protocol is GitHub Actions compatible
- [ ] `act_runner` binary embedding
- [ ] Minimal changes to GitHub client for Gitea API differences

### Configuration enhancements
- [ ] Per-repo image overrides
- [ ] Per-repo label overrides
- [ ] Environment variable overrides for all config fields (`EPHEMERD_` prefix)
- [ ] Config hot-reload on SIGHUP (repos, labels, concurrency)

### Firecracker microVM backend
- [ ] Mentioned in architecture doc as optional stronger isolation for Linux
- [ ] No code exists

### macOS base image tooling
- [ ] `ephemerd vm setup-macos` CLI command to provision base macOS VM image
- [ ] Download IPSW, boot VM, interactive or scripted provisioning, snapshot
- [ ] Currently users must create the base image manually

### Documentation
- [ ] Windows setup guide (Hyper-V, VHDX image, service install)
- [ ] macOS setup guide (base image creation, Virtualization.framework requirements)
- [ ] Homelab guide (Raspberry Pi, Mac Mini, mini PC recommendations)
- [ ] Troubleshooting guide (`ephemerd ctrctl`, common networking issues, log locations)
- [ ] Runner image customization guide (Dockerfile patterns for different languages/toolchains)

## Done

### Core runtime
- [x] In-process containerd server (k3s/rke2 model) with gRPC + tTRPC listeners
- [x] Embedded GitHub Actions runner binary via `go:embed` with platform detection and caching
- [x] JIT runner registration at repo and org level
- [x] Container lifecycle: create → wait → destroy with orphan cleanup on startup
- [x] Per-job OCI image selection via `EPHEMERD_IMAGE` env var (fetched from workflow YAML via API)
- [x] OCI artifact extraction for macOS VM jobs (pull image, extract layers to shared directory, cleanup)

### Job discovery
- [x] Polling mode (default, 10s interval, zero config)
- [x] Webhook mode (TLS, HMAC-SHA256 signature verification)
- [x] Localtunnel integration (vendored, opt-in via `tunnel = "localtunnel"`)
- [x] Org-level runners when `repos` config is omitted
- [x] Dedup by job ID with 10-minute TTL

### Scheduler
- [x] Concurrency limiter (semaphore channel)
- [x] Job timeout enforcement via `context.WithTimeout`
- [x] Graceful drain on SIGTERM with configurable shutdown timeout
- [x] Orphan container cleanup on startup
- [x] Ghost runner removal from GitHub on container creation failure
- [x] OOM/crash detection (exit code 137)
- [x] Health endpoint `/healthz` with JSON status
- [x] macOS VM job routing via `handleMacOSJob` (per-job Vz VMs with APFS clone-on-write)
- [x] Windows Linux job dispatch via WSL gRPC

### Networking
- [x] Linux: CNI bridge with NAT, iptables firewall blocking RFC 1918 + link-local
- [x] Windows: HCN NAT network with per-endpoint ACL policies
- [x] macOS: delegated to Linux VM (passthrough)
- [x] DNS resolution via `/etc/resolv.conf` bind mount

### Cross-platform
- [x] Linux VM on macOS via Virtualization.framework (containerd inside VM)
- [x] Linux VM on Windows via WSL2 (containerd + dispatch worker inside distro)
- [x] macOS per-job VMs via Virtualization.framework with APFS clone-on-write
- [x] macOS VM IP discovery via ARP table lookup with MAC normalization and subnet probing
- [x] macOS VM readiness detection via `.ready` sentinel file (SSH fallback)
- [x] macOS VM JIT config injection via virtio-fs shared directory
- [x] Hyper-V isolation for Windows containers
- [x] Cross-compilation: linux/amd64, linux/arm64, windows/amd64, darwin/arm64

### Security
- [x] Seccomp profile for Linux containers (`pkg/runtime/seccomp_linux.go`)
- [x] Container capability restrictions

### GitHub App authentication
- [x] GitHub App token flow with auto-refresh (`pkg/github/appauth.go`)
- [x] Wired through config: `app_id`, `installation_id`, `private_key_path`

### CLI
- [x] `ephemerd serve` — daemon with signal handling
- [x] `ephemerd status` — query health endpoint
- [x] `ephemerd drain` — trigger graceful shutdown
- [x] `ephemerd images` — list cached OCI images via containerd
- [x] `ephemerd config` — validate and display config
- [x] `ephemerd ctrctl` — containerd debug passthrough

### Build system
- [x] Mage build system with download, lint, test, build targets
- [x] Goreleaser config for cross-platform releases (`.goreleaser.yml`)
- [x] Runner version injected via ldflags
- [x] CI workflow: build + lint + test on push/PR (`.github/workflows/ci.yml`)
- [x] Release workflow (`.github/workflows/release.yml`)

### Unit tests (21 test files)
- [x] Config parsing and validation
- [x] GitHub App auth token refresh
- [x] GitHub webhook signature verification
- [x] EPHEMERD_IMAGE YAML parsing
- [x] Scheduler dedup logic
- [x] Artifact extraction
- [x] Network firewall rule generation
- [x] VM config defaults and MAC normalization
- [x] Runtime DNS and container lifecycle
- [x] Workflow platform detection
- [x] Localtunnel HTTP round-trip
