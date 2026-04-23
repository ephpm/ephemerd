# Docker-in-Docker Compatibility via Fake Docker API

## Problem

CI jobs frequently call `docker build`, `docker run`, and `docker push`. GitHub's hosted runners have a real Docker daemon available. Self-hosted runners behind ephemerd don't — containers run with restricted capabilities (no `CAP_SYS_ADMIN`) and there's no Docker daemon inside.

Users hit this when:
- Workflows call `docker build` to produce container images
- Workflows use `services:` to spin up databases/caches as sidecars
- Workflows call `docker run` to test images they just built
- Users install Docker themselves inside the job and expect it to work

Additionally, Forgejo/Gitea runners (`forgejo-runner`, `act_runner`) embed nektos/act which uses the Docker API to create **job containers** — the two-container model where the runner daemon spawns a separate container for each job via `docker create` + `docker exec`. This is a hard requirement for the forge integration, not just a nice-to-have for user workflows.

## Solution: Fake Docker Daemon in ephemerd

ephemerd serves a Docker Engine API on a Unix socket mounted into each job container at `/var/run/docker.sock`. The job's Docker CLI (pre-installed or user-installed) talks to this socket. ephemerd translates API calls into containerd operations on the host.

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

## API Coverage

There are two consumers of the fake socket with different needs:

1. **nektos/act** (inside forgejo-runner / act_runner) — needs exec, copy-to-container, and networking to run workflow steps inside job containers. This is the critical path for the Forgejo/Gitea integration.
2. **User workflows** — `docker run`, `docker build`, etc. called explicitly in `run:` steps. Nice-to-have for power users.

### What act calls during a job (in order)

```
1. GET  /_ping                              # health check
2. POST /images/create                      # pull job image (e.g. node:20-bookworm)
3. POST /containers/create                  # create job container
4. POST /containers/{id}/start              # start it
5. PUT  /containers/{id}/archive            # copy step script into container
6. POST /containers/{id}/exec               # create exec for step
7. POST /exec/{id}/start                    # attach + stream stdout/stderr
8. GET  /exec/{id}/json                     # get exit code
   ... repeat 5-8 for each workflow step ...
9. GET  /containers/{id}/archive            # copy artifacts out (optional)
10. POST /containers/{id}/stop              # stop container
11. DELETE /containers/{id}                 # remove container
```

For workflows with `services:` (databases, caches):

```
    POST /networks/create                   # bridge network for job + services
    POST /containers/create                 # per-service container
    POST /containers/{id}/start             # start service
    POST /networks/{id}/connect             # connect job + service to network
    ... job runs ...
    DELETE /containers/{id}                 # remove service containers
    DELETE /networks/{id}                   # remove network
```

### Implementation status

| Endpoint | Status | Consumer | Notes |
|---|---|---|---|
| `GET /_ping` | **Done** | act, CLI | Returns `OK` with API version headers |
| `GET /version` | **Done** | act, CLI | Returns `27.0.0-ephemerd` |
| `GET /info` | **Done** | act, CLI | Reports container/image counts |
| `GET /images/json` | **Done** | CLI | Lists pulled images |
| `POST /images/create` (pull) | **Done** | act, CLI | Pulls via containerd, streams progress JSON |
| `POST /containers/create` | **Done** | act, CLI | Creates containerd container with OCI spec |
| `POST /containers/{id}/start` | **Done** | act, CLI | Creates task, attaches CNI, starts process |
| `GET /containers/{id}/json` (inspect) | **Done** | act, CLI | Returns state, IP, config |
| `POST /containers/{id}/stop` | **Done** | act, CLI | SIGTERM → SIGKILL with timeout |
| `POST /containers/{id}/wait` | **Done** | act, CLI | Blocks until exit, returns status code |
| `GET /containers/{id}/logs` | **Done** | CLI | Returns stdout/stderr from log file |
| `DELETE /containers/{id}` | **Done** | act, CLI | Full cleanup: task, network, snapshot |
| `GET /containers/json` (list) | **Done** | CLI | Lists all containers with state |
| `POST /containers/{id}/exec` | **Done** | act | Create exec process via containerd task.Exec() |
| `POST /exec/{id}/start` | **Done** | act | Start exec, block until exit, return output |
| `GET /exec/{id}/json` | **Done** | act | Return running state and exit code |
| `PUT /containers/{id}/archive` | **Done** | act | Copy tar into container (via exec tar or upperdir) |
| `GET /containers/{id}/archive` | **Done** | act | Copy tar out from container rootfs layers |
| `GET /images/{name}/json` | **TODO** | act | Check if image exists before pulling |
| `POST /networks/create` | **TODO** | act | Needed for `services:` |
| `POST /networks/{id}/connect` | **TODO** | act | Needed for `services:` |
| `DELETE /networks/{id}` | **TODO** | act | Cleanup |
| `POST /build` | **Done** | CLI | Buildah library (`imagebuildah.BuildDockerfiles`) in-process |
| `POST /images/{name}/push` | **TODO** | CLI | Needs buildah + registry auth |
| `POST /images/{name}/tag` | **TODO** | CLI | Alias in in-memory map |
| `DELETE /images/{name}` | **TODO** | CLI | Remove from map |

### Priority tiers

**Tier 1 — Required for act job execution** (done):
- ~~`POST /containers/{id}/exec` + `POST /exec/{id}/start` + `GET /exec/{id}/json`~~ — step execution
- ~~`PUT /containers/{id}/archive`~~ — copy step scripts into container

**Tier 2 — Required for `services:` support** (databases, caches as sidecars):
- `POST /networks/create` + `POST /networks/{id}/connect` + `DELETE /networks/{id}`

**Tier 3 — User workflow convenience** (explicit `docker` commands in `run:` steps):
- `GET /images/{name}/json` — check local image before pull
- `GET /containers/{id}/archive` — copy files out
- `POST /build` — image builds via buildah
- `POST /images/{name}/push` — push to registry

**Tier 4 — Nice to have**:
- `POST /images/{name}/tag`
- `DELETE /images/{name}`
- `docker compose` (client-side tool, may work if it only uses implemented endpoints)
- `docker volume create` (use bind mounts instead)

### Not planned

| Docker API | Reason |
|---|---|
| Swarm APIs | Not applicable |
| Kubernetes APIs | Not applicable |
| Plugin APIs | Not applicable |
| `docker checkpoint` | Requires CRIU, not useful for CI |

## API Translation Details

Each job gets its own fake daemon instance. The daemon maintains an in-memory image store (`map[string]*image`) and a container map (`map[string]*containerEntry`). All state is scoped to the job and destroyed when the job exits.

### Image operations

| Docker API | ephemerd action |
|---|---|
| `POST /images/create` (pull) | Check shared `ephemerd` namespace first (cached base images). Pull into per-job namespace on miss. Register in the in-memory map. Stream progress JSON. |
| `GET /images/json` | Return entries from the in-memory map. |
| `POST /build` | Extract tar build context, call `imagebuildah.BuildDockerfiles()` in-process via buildah library. Uses per-job `containers/storage` root for isolation. Register result in in-memory map. |
| `POST /images/{name}/push` | *(planned)* Push via `buildah push` with registry credentials. |

### Container lifecycle

| Docker API | ephemerd action |
|---|---|
| `POST /containers/create` | Resolve image (pull if needed). Build OCI spec from request body (Cmd, Env, WorkingDir, Binds). Create containerd container with overlayfs snapshot. Return 64-char hex ID. |
| `POST /containers/{id}/start` | Create containerd task with log capture (`cio.LogFile`). Attach CNI networking to task's network namespace. Start task. Container gets IP on the ephemerd bridge. |
| `GET /containers/{id}/json` | Return state (created/running/exited), exit code, IP address, config. Status refreshed from containerd task on each call. |
| `POST /containers/{id}/stop` | Send SIGTERM, wait up to 10s, then SIGKILL. Update status to exited. |
| `POST /containers/{id}/wait` | Block on containerd task exit channel. Return `{"StatusCode": N}`. |
| `GET /containers/{id}/logs` | Read and return the log file written by `cio.LogFile`. |
| `DELETE /containers/{id}` | Kill task if running, delete task, teardown CNI, delete container + snapshot, remove log files. |
| `GET /containers/json` | List all containers with state and network info. |

### Exec operations

| Docker API | ephemerd action |
|---|---|
| `POST /containers/{id}/exec` | Build OCI process spec from request (Cmd, Env, WorkingDir). Inherit container env. Create exec via `task.Exec()` with `cio.LogFile`. Return exec ID. |
| `POST /exec/{id}/start` | Register `Wait()` channel, call `Start()`, block until process exits. Return stdout/stderr from log file as response body. Does not use Docker's connection hijacking — sufficient for act's usage pattern. |
| `GET /exec/{id}/json` | Return `{"ExitCode": N, "Running": bool}`. Refreshes state from containerd on each call. |

### Copy operations

| Docker API | ephemerd action |
|---|---|
| `PUT /containers/{id}/archive` | For running containers: write tar to overlay upperdir, then exec `tar xf` inside the container. For stopped containers: extract directly into the overlay upperdir. Path traversal prevention via `filepath.Clean`. |
| `GET /containers/{id}/archive` | Search overlay upperdir then lowerdirs for the requested path. Tar up matching files and stream back. |

### Health / metadata

| Docker API | ephemerd action |
|---|---|
| `GET /_ping` | Return `OK` with `API-Version: 1.45` header. |
| `GET /version` | Return version info (`27.0.0-ephemerd`, API `1.45`). |
| `GET /info` | Return system info with live container/image counts. |

## Socket Lifecycle

1. **Job starts**: ephemerd creates a Unix socket at `<DataDir>/jobs/<JobID>/docker/d.sock`, starts the fake daemon goroutine, mounts the socket into the container at `/var/run/docker.sock`.
2. **Job runs**: Docker CLI / act inside the container talks to the socket. ephemerd handles requests, creates sibling containers on the CNI bridge, tracks all state per-job.
3. **Job finishes**: ephemerd calls `destroyAllContainers()` — kills tasks, tears down CNI, deletes snapshots, removes log directories. Then closes the socket and removes the docker directory. No leaked state.

## Sibling Containers

Containers created via the fake socket are **sibling containers** — they run alongside the job container as first-class containerd containers, not nested. They share the same CNI bridge (`ephemerd0`) and firewall rules. From the job's perspective, siblings are reachable at their container IP (10.88.x.x).

Name resolution for `--name` containers (e.g. `docker run -d --name postgres postgres:16`) can be supported by injecting `/etc/hosts` entries into the job container, mapping the name to the sibling's IP.

This is also how `services:` in workflow YAML works — act calls `docker create` + `docker start` for each service, and those calls hit the fake daemon which creates sibling containers on the shared network.

## Namespace Isolation

DinD operations use a **two-tier containerd namespace** model to prevent image leaks between jobs while preserving caching for common base images:

```
Shared namespace: "ephemerd"
  └── Runner containers (actions-runner, forgejo-runner images)
  └── Base images cached by ephemerd (node:20, ubuntu:24.04, etc.)

Per-job namespace: "ephemerd/dind/{jobID}"
  └── Images pulled via fake socket (docker pull)
  └── Images built via fake socket (docker build)
  └── Containers created via fake socket (docker run)
```

**Image pull read-through**: when a job does `docker pull node:20`, the fake socket checks the shared `ephemerd` namespace first. If the image exists there (pulled by ephemerd for runner containers), it's referenced directly — no redundant pull. Private registry images that aren't in the shared namespace are pulled into the per-job namespace, invisible to other jobs.

**Build isolation**: `docker build` output goes into a per-job `containers/storage` root at `<DataDir>/jobs/<JobID>/docker/buildah-store/`. This is completely isolated from other jobs and from containerd's store.

**Cleanup**: when the job exits, the entire per-job namespace is destroyed — all containers, snapshots, images, and build artifacts. The shared namespace is untouched.

**Why per-job, not per-repo**: while per-repo namespaces would improve cache hit rates, they create a window where one PR's build artifacts are visible to concurrent jobs in the same repo. Per-job namespaces are the strictest isolation boundary. Cross-job caching can be added later via a read-only shared layer.

## Storage

- **Pulled images (shared)**: common base images in containerd's `ephemerd` namespace, cached across all jobs.
- **Pulled images (private)**: per-job images in containerd's `ephemerd/dind/{jobID}` namespace. Destroyed on job exit.
- **Container snapshots**: one overlayfs snapshot per container, named `{containerID}-snapshot`. Cleaned up on container removal.
- **Container logs**: per-container log files at `<DataDir>/jobs/<JobID>/docker/containers/<containerID>/output.log`. Cleaned up on removal.
- **Build layers**: per-job `containers/storage` at `<DataDir>/jobs/<JobID>/docker/buildah-store/`. Destroyed on job exit.
- **Garbage collection**: on job cleanup, `destroyAllContainers()` handles containers, `os.RemoveAll` handles the buildah store directory. Shared images remain cached.

## Dependencies

- **containerd**: embedded, provides image pull and container lifecycle.
- **CNI plugins**: bridge + host-local + portmap, extracted at build time. Provide networking for sibling containers.
- **buildah**: imported as Go library (`github.com/containers/buildah`). Used for `docker build`. Built with `containers_image_openpgp` tag to avoid CGo dependency on gpgme.
- **No Docker daemon**: the entire point.
- **No additional capabilities**: sibling containers get the same restricted capability set as job containers.

## Implementation Order

1. ~~Socket server + health endpoints (`/_ping`, `/version`)~~ — done
2. ~~Image pull (`POST /images/create`)~~ — done
3. ~~Container create/start/stop/wait/inspect/delete~~ — done
4. ~~CNI networking for sibling containers~~ — done
5. ~~Exec (create/start/inspect)~~ — done
6. ~~Copy to/from container~~ — done
7. ~~Image build (`POST /build`) — buildah as Go library~~ — done
8. ~~Per-job containerd namespace isolation~~ — done
9. **Image inspect** (`GET /images/{name}/json`) — act checks before pulling
10. **Network create/connect** — required for `services:` support
11. **Image push** — registry auth + `buildah push`
12. **`/etc/hosts` injection** — service discovery for named sidecars
