# Docker-in-Docker Compatibility via Fake Docker API

## Problem

CI jobs frequently call `docker build`, `docker run`, and `docker push`. GitHub's hosted runners have a real Docker daemon available. Self-hosted runners behind ephemerd don't — containers run with restricted capabilities (no `CAP_SYS_ADMIN`) and there's no Docker daemon inside.

Users hit this when:
- Workflows call `docker build` to produce container images
- Workflows use `services:` to spin up databases/caches as sidecars
- Workflows call `docker run` to test images they just built
- Users install Docker themselves inside the job and expect it to work

## Solution: Fake Docker Daemon in ephemerd

ephemerd serves a Docker Engine API on a Unix socket mounted into each job container at `/var/run/docker.sock`. The job's Docker CLI (pre-installed or user-installed) talks to this socket. ephemerd translates API calls into containerd/buildah operations on the host.

No actual Docker daemon runs. No privileged containers. No `CAP_SYS_ADMIN`.

## Architecture

```
┌─────────────────────────────────┐
│  Job Container                  │
│                                 │
│  docker build -t myapp .        │
│       │                         │
│       ▼                         │
│  /var/run/docker.sock           │
└───────────┬─────────────────────┘
            │ Unix socket
            ▼
┌─────────────────────────────────┐
│  ephemerd (fake Docker API)     │
│                                 │
│  POST /build → buildah bud      │
│  POST /containers/create → ctr  │
│  POST /images/create → ctr pull │
│  POST /images/push → buildah    │
│                                 │
│  In-memory image map per job    │
│  Temp OCI layer storage on disk │
└───────────┬─────────────────────┘
            │
            ▼
┌─────────────────────────────────┐
│  containerd (embedded)          │
│                                 │
│  Pulls remote images            │
│  Creates sibling containers     │
│  Same network, same firewall    │
└─────────────────────────────────┘
```

## API Translation

Each job gets its own fake daemon instance. The daemon maintains an in-memory image store (`map[string]*image`) and a temp directory for OCI layers. All state is scoped to the job and destroyed when the job exits.

### Image builds

| Docker API | ephemerd action |
|---|---|
| `POST /build` | Stream build context to a temp dir. Run `buildah bud` with the context. Store resulting OCI image in temp storage. Register image name in the in-memory map. Return image ID. |
| `POST /build` with `--target` | Buildah handles multi-stage natively via `bud --target`. |

### Image management

| Docker API | ephemerd action |
|---|---|
| `POST /images/create` (pull) | Pull via containerd's content store. Register in the in-memory map. |
| `POST /images/{name}/push` | Push to real remote registry via `buildah push`. Uses registry credentials from the job's environment or `~/.docker/config.json` if present. |
| `POST /images/{name}/tag` | Add an alias in the in-memory map. No data copied. |
| `GET /images/json` | Return entries from the in-memory map. |
| `GET /images/{name}/json` | Inspect from the in-memory map. |
| `DELETE /images/{name}` | Remove from map, optionally clean layers from temp dir. |

### Container operations (sidecars)

| Docker API | ephemerd action |
|---|---|
| `POST /containers/create` | Look up image in the in-memory map (local build) or pull via containerd (remote image). Create a sibling container on the same CNI network as the job. Apply the same firewall rules and capability restrictions. Return container ID. |
| `POST /containers/{id}/start` | Start the containerd task. |
| `POST /containers/{id}/stop` | Send SIGTERM, then SIGKILL after grace period. |
| `DELETE /containers/{id}` | Destroy container and snapshot via containerd. |
| `GET /containers/{id}/json` | Return container state from containerd. |
| `GET /containers/{id}/logs` | Stream logs from containerd's log pipe. |
| `POST /containers/{id}/wait` | Block until containerd task exits. |

### Health / metadata

| Docker API | ephemerd action |
|---|---|
| `GET /_ping` | Return `OK`. |
| `GET /version` | Return a fake version response (buildah version as `ApiVersion`). |
| `GET /info` | Return minimal system info. |

### Not supported

| Docker API | Reason |
|---|---|
| `docker compose` | Too complex — compose is a client-side tool that makes many API calls. Basic `docker compose up` may work if it only uses `create` + `start`, but no guarantees. |
| `docker exec` | Requires `CAP_SYS_PTRACE` or direct task access. Could be supported later via containerd's exec API. |
| `docker network create` | Jobs share the ephemerd CNI network. Custom networks are not supported. Containers are reachable by IP on the shared subnet. |
| `docker volume create` | Use bind mounts from the job's workspace instead. |
| Swarm / Kubernetes APIs | Not applicable. |

## Socket Lifecycle

1. **Job starts**: ephemerd creates a Unix socket at a temp path, starts the fake daemon goroutine, mounts the socket into the container at `/var/run/docker.sock`.
2. **Job runs**: Docker CLI in the container talks to the socket. ephemerd handles requests, creates sidecars as sibling containers, tracks everything in the per-job state.
3. **Job finishes**: ephemerd destroys all sibling containers created by this job, deletes the temp OCI layer directory, closes the socket. No leaked state.

## Sibling Containers

Sidecars created via `docker run` are **sibling containers** — they run alongside the job container, not inside it. They share the same CNI network and firewall rules. From the job's perspective, sidecars are reachable at their container IP.

For service discovery, ephemerd can inject environment variables or `/etc/hosts` entries into the job container when a sidecar is created with `--name`. For example, `docker run -d --name mysql mysql:8` would add `mysql → 10.88.x.x` to the job's `/etc/hosts`.

This also enables `services:` in workflow YAML to work — the GitHub runner binary calls `docker` under the hood to manage service containers, and those calls would hit our fake daemon.

## Storage

- **Build layers**: stored as OCI directories under `<data-dir>/jobs/<job-id>/layers/`. Buildah's `--root` and `--runroot` flags point here.
- **Pulled images**: stored in containerd's normal content store (shared across jobs for caching).
- **Garbage collection**: when the job is destroyed, `os.RemoveAll` on the job's layer directory. Pulled images remain in containerd's store for future jobs.

## Dependencies

- **buildah**: embedded or downloaded at build time (like golangci-lint). Statically linked binary, ~30MB.
- **No Docker daemon**: the entire point.
- **No additional capabilities**: buildah runs rootless. Sidecar containers get the same restricted capability set as job containers.

## Implementation Order

1. **Socket server + health endpoints** (`/_ping`, `/version`) — proves the socket mount works
2. **Image pull** (`POST /images/create`) — containerd integration
3. **Container create/start/stop/delete** — sidecar lifecycle via containerd
4. **Image build** (`POST /build`) — embed buildah, wire up build context streaming
5. **Image push** — registry auth + `buildah push`
6. **`/etc/hosts` injection** — service discovery for sidecars
7. **`services:` YAML support** — test that the runner binary's service management works through the shim
