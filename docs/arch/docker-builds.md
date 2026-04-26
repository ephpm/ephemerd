# docker builds in ephemerd

## Status

Draft — under review.

## Context

ephemerd jobs increasingly need `docker build` and `docker buildx build` to
work inside CI containers. Today we don't support this first-class: the fake
Docker daemon in `pkg/dind` implements enough of the Docker API to let runner
tools (forgejo-runner, act_runner) create job containers, but it does not
implement `POST /build`. Users who want to build images inside an ephemerd job
have two bad choices:

1. Run `docker buildx build` against `moby/buildkit:buildx-stable-1-*` — worked
   historically, now broken because moby stopped publishing Windows buildkit
   images in any form. The Windows CI runner image build workflow
   (`.github/workflows/build-images.yml`) fails for this reason.
2. Point `DOCKER_HOST` at a real Docker daemon on the host — breaks ephemerd's
   self-contained model and pollutes host state.

Linux already has a viable-ish path (buildx + `docker-container` driver with
the retired Linux tag still working today), but the same upstream-dependency
risk applies. Windows is the forcing function.

## Decision

**Embed BuildKit as a Go library inside ephemerd, and extend `pkg/dind` to
translate Docker build API calls into BuildKit solve requests.** Same
architectural model as the existing in-process containerd integration:
ephemerd owns the build engine end-to-end, with no subprocess, no separate
binary, no external daemon dependency.

`docker build` inside a job container hits `pkg/dind`'s Docker API endpoint
like it hits any other Docker call today. `pkg/dind` translates to BuildKit
gRPC, the embedded BuildKit solver produces the image against ephemerd's
embedded containerd, and the result streams back as Docker-shaped JSON line
progress.

## Architecture

```
┌─────────────────────────── ephemerd process ────────────────────────────┐
│                                                                         │
│   ┌───────────────┐        ┌────────────────────────────────────────┐  │
│   │               │        │  BuildKit (embedded)                   │  │
│   │   containerd  │◄───────┤   - control.Controller                 │  │
│   │   (embedded)  │        │   - worker/containerd.NewWorkerOpt     │  │
│   │               │        │   - frontend/dockerfile.Build          │  │
│   └───────────────┘        └──────────────┬─────────────────────────┘  │
│         ▲                                 ▲                             │
│         │ OCI image out                   │ local gRPC                   │
│         │                                 │                             │
│   ┌─────┴─────────────────────────────────┴──────────────────────────┐  │
│   │  pkg/dind                                                        │  │
│   │   - existing: Docker container/image API → containerd            │  │
│   │   - new:     Docker build API → BuildKit                         │  │
│   │     · POST /build                                                │  │
│   │     · POST /session (grpc-over-http hijack for --secret/--ssh)   │  │
│   │     · Dockerfile option parse + SolveOpt construction            │  │
│   │     · buildkit Progress → docker jsonmessage translation         │  │
│   └──────────────────────────────────────────────────────────────────┘  │
│                          ▲                                              │
└──────────────────────────┼──────────────────────────────────────────────┘
                           │ Docker API (npipe on Windows, unix on Linux)
                           │
                  ┌────────┴─────────┐
                  │ CI job container │
                  │  $ docker build  │
                  └──────────────────┘
```

## Trust boundary and security

Embedding BuildKit in-process means the BuildKit daemon code (solver,
Dockerfile frontend parser, session gRPC server) runs with ephemerd's
privilege on the host. This is the same trust shape ephemerd already accepts
for embedded containerd. It is **not** the same as running untrusted Docker
build commands on the host: every `RUN` step still executes inside an
isolated worker container.

### What's on the host
- BuildKit's solver, frontend parser, session gRPC server
- Option translation glue in `pkg/dind`
- Same process as embedded containerd

### What's isolated (per RUN step)
- Linux: OCI container via containerd + runc. Same isolation as any ephemerd
  job container — process namespaces, cgroups, (optionally) user namespace
  remapping.
- Windows: Hyper-V isolated container per `RUN`. Separate kernel per step;
  the strongest per-step isolation available on a single host.

### New host-side attack surfaces
1. Dockerfile parser bugs (`frontend/dockerfile`) on untrusted input →
   ephemerd-process compromise.
2. BuildKit solver / gateway bugs on untrusted Dockerfiles or gateway
   frontends → ephemerd-process compromise.
3. Session gRPC server bugs when jobs POST `/session` → ephemerd-process
   compromise.

Each is a zero-day-in-a-parser risk, qualitatively similar to the existing
risk of containerd processing untrusted image manifests.

### Default mitigations
- `security.insecure` entitlement (host-privileged RUN) — **disabled**.
- `network.host` entitlement (RUN step on host network) — **disabled**.
- Custom gateway frontends (`# syntax = <arbitrary-image>`) — allowed by
  default (required for standard Dockerfile syntax); disabled pathway
  available via `--allow=` opt-in if a deployment wants stricter posture.
- Resource caps on each build: CPU / memory / disk quota enforced through
  the worker. Per-job, not per-RUN, for simplicity.
- Hyper-V isolation on Windows is non-negotiable; if HCS ever falls back to
  process-isolated containers, build fails rather than running unisolated.
- Version-pin BuildKit; upgrade is a deliberate release, not an automatic
  dependency bump.

### Hardening roadmap
The **in-process** model is phase 1–3. For operators who want to shrink
the on-host BuildKit trust boundary, phase 4 runs BuildKit in a sidecar
container that ephemerd owns and tears down — BuildKit parser bugs then
compromise a disposable container, not the host. See Phase 4 below.

## Components

### 1. BuildKit library integration

**Imports:**
- `github.com/moby/buildkit/control` — the Controller (gRPC solver entry point)
- `github.com/moby/buildkit/worker/base`, `worker/containerd` — containerd-backed worker
- `github.com/moby/buildkit/frontend/dockerfile/builder` — Dockerfile frontend (the `Build` function is registered as a named frontend on the Controller)
- `github.com/moby/buildkit/session` — session manager (auth, secrets, SSH forwarding)
- `github.com/moby/buildkit/solver/bboltcachestorage` — persistent cache store

**Wiring:**
- Construct a `worker.Controller` pointing at ephemerd's embedded containerd (via the same socket/named pipe ephemerd already uses internally).
- Register the Dockerfile frontend plus the `gateway.v0` frontend (for `--frontend=` overrides from clients).
- Cache store lives in `<dataDir>/buildkit/` (or `<dataDir>\buildkit\` on Windows).
- History DB + provenance support in phase 2 — initial cut disables provenance to keep scope small.
- Controller runs on an internal gRPC listener (not exposed off-box). `pkg/dind` dials it as a library client.

**Linux worker path:** containerd with the `overlayfs` snapshotter, `runc` runtime. Works today; BuildKit has linux/amd64 + linux/arm64 first-class.

**Windows worker path:** containerd with the `windows` snapshotter, `runhcs` runtime (Hyper-V-isolated job containers per `RUN` step). BuildKit's Windows support is marked experimental upstream but has been shipping for multiple releases. **We accept the experimental label and pin to a known-good buildkit version; see Open Questions.**

**macOS:** not in scope. macOS jobs run inside the Linux VM and hit the Linux path.

### 2. Docker build API in `pkg/dind`

**Endpoints to add:**
- `POST /build` — multipart or tarred-context upload, Docker build options via query string / headers
- `POST /session` — hijacked HTTP → gRPC for session-scoped aux channels (`--secret`, `--ssh`, registry auth, filesync for `ADD`/`COPY`)

**Option translation (Docker build form → BuildKit `SolveOpt`):**
| Docker option | BuildKit equivalent |
|---|---|
| `-t name:tag` | `Exports[].Attrs["name"]` with image exporter |
| `-f Dockerfile` | `FrontendAttrs["filename"]` |
| `--build-arg K=V` | `FrontendAttrs["build-arg:K"] = V` |
| `--target stage` | `FrontendAttrs["target"]` |
| `--platform list` | `FrontendAttrs["platform"]` |
| `--secret id=…` | session attachable secret |
| `--ssh default` | session attachable SSH forwarder |
| `--cache-from`/`--cache-to` | `CacheImports` / `CacheExports` |
| `--no-cache` | `FrontendAttrs["no-cache"]` |
| `--pull` | `FrontendAttrs["image-resolve-mode"]="pull"` |
| `--label K=V` | `FrontendAttrs["label:K"] = V` |

**Streaming response:** BuildKit emits `client.SolveStatus` messages on a channel. We translate each to Docker's `jsonmessage.JSONMessage` line format and stream on the HTTP response body, matching what `docker build` expects to render.

### 3. Session server

Docker clients `POST /session` to establish a gRPC stream tunneled over HTTP. We implement the server side using BuildKit's own `session.Manager`:
- attachable secret store (`secrets.SecretStore`)
- SSH forwarder (`sshforward.SSHServer`)
- file sync (`filesync.FileSyncServer`)
- auth provider for registry credentials (`auth.AuthServer`)

Every solve request references a session ID; BuildKit pulls attachables off that session during the solve.

### 4. Image output + tagging

BuildKit's image exporter writes the final image into containerd's image store under the tag we were asked for. Subsequent `docker push <tag>` hits the existing `pkg/dind` push path, which already knows how to talk to containerd + the registry. No new code on the push side.

## Scope (phased)

### Phase 1 — MVP build
- BuildKit library wired up against ephemerd's embedded containerd (Linux first, Windows immediately after)
- `POST /build` with: `-t`, `-f`, `--build-arg`, `--target`, `--platform`, `--no-cache`, `--pull`, `--label`
- Dockerfile frontend
- Progress streaming in Docker jsonmessage format
- Basic error mapping
- Unit tests for the option-translation layer
- Integration test: build a tiny Alpine-based image inside a Linux job; build a servercore-based image inside a Windows job

### Phase 2 — auxiliary features
- Session server: `--secret`, `--ssh`, registry auth passthrough
- `--cache-from` / `--cache-to` (local and registry cache backends)
- `gateway.v0` frontend support (for `# syntax = …` custom frontends)
- Multi-platform builds (single-invocation manifest list)

### Phase 3 — buildx and ergonomics
- Expose BuildKit gRPC on a second named pipe / unix socket so `docker buildx` can attach as a remote builder
- Ship an `ephemerd buildx driver` binary or a provisioning helper so workflows can `docker buildx create --driver remote ...`
- Build cache inspection / prune commands (via `pkg/dind` or a new `ephemerd cache` CLI subcommand)

### Phase 4 — BuildKit in a sidecar container (opt-in hardening)
For deployments that want BuildKit parser bugs to compromise a disposable
container rather than the ephemerd host, ephemerd can optionally run
BuildKit *out-of-process* in a sidecar container it owns.

- ephemerd spawns a BuildKit sidecar container on first `docker build`
  request (or proactively at `--dind` startup, configurable).
- Sidecar image: an ephemerd-owned Windows/Linux buildkit image
  (`ghcr.io/ephpm/buildkit:<platform>-<version>`). Chicken-and-egg: bootstrap
  each image once via a `windows-latest` / `ubuntu-latest` GitHub-hosted
  workflow; subsequent rebuilds run under the ephemerd fleet itself.
- `pkg/dind`'s Docker build endpoint stays the same to the job; internally
  it proxies to the sidecar over gRPC instead of to the in-process solver.
- Sidecar tears down when idle (configurable timeout) or at ephemerd exit.
- Same containerd worker underneath — sidecar talks to ephemerd's containerd
  via the existing socket/named-pipe.

Implementation cost is mostly the sidecar image + lifecycle management; the
`pkg/dind` build handler and option-translation layer from phases 1–2 are
reused unchanged (the only difference is which gRPC endpoint the handler
dials).

Flag: `--buildkit-sidecar` (off by default). When off, phases 1–3 behavior;
when on, route through sidecar.

## Open questions

1. **BuildKit Windows stability.** Upstream still flags Windows as experimental. How recent has the worker/containerd-on-Windows been exercised in CI? We need to pin a version where it's known-good, and we own the upgrade decision. Proposal: pin to the latest minor (v0.29.x as of this writing), upgrade on our own cadence.
2. **Binary size.** Measured on Windows amd64 with buildkit v0.25.1: linking `pkg/buildkit` adds **~7.9 MB** to ephemerd (126.3 MB → 134.6 MB, both built with empty embed placeholders). Smaller than the 20–35 MB I initially estimated — ephemerd already transitively pulls buildkit's dep graph through buildah/podman, so the marginal cost is only the subpackages we directly reference (control, solver, worker/containerd, frontend/dockerfile, session). Linux likely comparable. **Resolved.**
3. **Control.Controller stability.** `control` package is pre-v1; BuildKit reserves the right to change internal API. Mitigate with vendoring + tight version pin; flag upgrade-breaks via integration tests.
4. **CDI device manager.** `worker.Controller` takes a `CDIManager` — we probably want to pass nil for now. Confirm it's optional and doesn't crash the path.
5. **Provenance / attestations.** BuildKit emits provenance attestations by default in recent versions. Initial cut should disable these (simpler output, smaller manifest). Users who want them can enable via build args later.
6. **Cache persistence.** Should the buildkit cache persist across ephemerd restarts, or wipe per-job? Defer to phase 2 — start with per-daemon-lifetime cache.

## Alternatives considered

### A. Subprocess `dockerd.exe`
Embed the Windows `dockerd.exe` binary in ephemerd, start it as a child process in `--dind` mode, proxy the job-side `/build` call to it.

- ✅ Works today with Docker's own battle-tested code
- ✅ No new BuildKit integration to write
- ❌ +50 MB binary bloat (Windows only; Linux still embeds containerd directly)
- ❌ Subprocess lifecycle to babysit: start/stop/crash recovery
- ❌ We inherit Docker's build feature surface wholesale, including features we don't want
- ❌ Two different build paths per platform (Linux in-process buildkit, Windows proxied dockerd) — asymmetric and harder to maintain

### B. Host-installed Docker Engine detection
Document "install Docker Engine for Windows Server on the ephemerd host if you want `docker build` in jobs"; `pkg/dind` detects it and proxies.

- ✅ Trivial code change
- ❌ Host platform dependency — ephemerd no longer self-contained on Windows
- ❌ State pollution: user builds accumulate in the host's daemon image cache
- ❌ DockerMsftProvider (the free install path) is pinned to Docker 20.10 from 2021
- ❌ Licensing hook: Docker Desktop is commercial-gated for larger orgs

### C. Link `moby/moby` (dockerd) as a Go library
Import the dockerd build routes directly instead of the BuildKit control layer.

- ❌ moby/moby is not structured as a library — internal packages, circular deps, daemon assumptions baked into every package
- ❌ Massive dependency graph (probably triples binary size)
- ❌ We'd inherit Docker's CVE treadmill for the entire daemon, not just the build path
- Previous attempts by other projects have been abandoned

### D. `moby/buildkit:buildx-stable-1-windowsservercore-ltsc2022` via `docker-container` driver
The original pattern.

- ❌ moby retired Windows image tags; no replacement at any tag on Docker Hub
- ❌ Even when it worked, nested containerd-in-a-Windows-container required fragile hypervisor-nesting / shared-HCS tricks — exactly why moby dropped publishing

## Out of scope

- Legacy (non-BuildKit) `docker build`. Modern Docker clients default to BuildKit since 23.0; we support only the BuildKit path.
- BuildKit's containerd-backed features that don't apply to us (e.g., Kubernetes worker pool).
- Signing / SBOM attestations beyond what BuildKit emits by default.

## Migration

No migration step. Existing `docker build` calls in jobs start working the
first ephemerd version that ships phase 1. Pre-existing workarounds (jobs that
shelled out to `docker buildx build` with a remote driver) continue to work
because `pkg/dind` still exposes the buildkit gRPC for them, once phase 3
lands.

## Follow-ups to pre-work before phase 1 starts

- Fix CI's `build-images.yml` Windows job in the meantime by pointing it at
  GitHub's `windows-latest` hosted runner (unblocks the CI image build
  pipeline that's broken today; see the `feat-multi-provider` CI fix for the
  shape of a similar unblock).
- Spike BuildKit library embedding in a throwaway branch: construct a
  Controller, solve a hard-coded "FROM alpine; RUN echo hi" build against our
  containerd, measure binary size delta. Answers open questions 1 and 2.
