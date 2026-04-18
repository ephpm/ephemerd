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
| `POST /containers/{id}/exec` | **TODO** | act | **Critical for act** — create exec session |
| `POST /exec/{id}/start` | **TODO** | act | **Critical for act** — attach + stream I/O |
| `GET /exec/{id}/json` | **TODO** | act | **Critical for act** — get exit code |
| `PUT /containers/{id}/archive` | **TODO** | act | **Critical for act** — copy files into container |
| `GET /containers/{id}/archive` | **TODO** | act | Copy files out (artifacts) |
| `GET /images/{name}/json` | **TODO** | act | Check if image exists before pulling |
| `POST /networks/create` | **TODO** | act | Needed for `services:` |
| `POST /networks/{id}/connect` | **TODO** | act | Needed for `services:` |
| `DELETE /networks/{id}` | **TODO** | act | Cleanup |
| `POST /build` | **TODO** | CLI | Needs buildah integration |
| `POST /images/{name}/push` | **TODO** | CLI | Needs buildah + registry auth |
| `POST /images/{name}/tag` | **TODO** | CLI | Alias in in-memory map |
| `DELETE /images/{name}` | **TODO** | CLI | Remove from map |

### Priority tiers

**Tier 1 — Required for act job execution** (without these, no workflow steps run):
- `POST /containers/{id}/exec` + `POST /exec/{id}/start` + `GET /exec/{id}/json` — step execution
- `PUT /containers/{id}/archive` — copy step scripts into container

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
| `POST /images/create` (pull) | Pull via containerd's content store. Register in the in-memory map. Stream progress JSON. |
| `GET /images/json` | Return entries from the in-memory map. |
| `POST /build` | *(planned)* Stream build context to temp dir. Run `buildah bud`. Register result. |
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

### Exec operations (planned)

| Docker API | ephemerd action |
|---|---|
| `POST /containers/{id}/exec` | *(planned)* Create exec spec with Cmd, Env, WorkingDir. Return exec ID. Will use containerd's `task.Exec()` to create an additional process in the container's namespaces. |
| `POST /exec/{id}/start` | *(planned)* Start the exec process, stream stdout/stderr via hijacked connection. Act expects a raw TCP stream after the HTTP upgrade. |
| `GET /exec/{id}/json` | *(planned)* Return `{"ExitCode": N, "Running": false}` from the completed exec process. |

### Copy operations (planned)

| Docker API | ephemerd action |
|---|---|
| `PUT /containers/{id}/archive` | *(planned)* Accept a tar stream, extract into the container's rootfs at the specified path. Requires access to the container's mount namespace or snapshot mount point. |
| `GET /containers/{id}/archive` | *(planned)* Tar up files from the container's rootfs and stream back. |

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

## Storage

- **Pulled images**: stored in containerd's content store (shared across jobs for caching).
- **Container snapshots**: one overlayfs snapshot per container, named `{containerID}-snapshot`. Cleaned up on container removal.
- **Container logs**: per-container log files at `<DataDir>/jobs/<JobID>/docker/containers/<containerID>/output.log`. Cleaned up on removal.
- **Build layers** *(planned)*: OCI directories under `<DataDir>/jobs/<JobID>/layers/`. Buildah's `--root` and `--runroot` flags point here.
- **Garbage collection**: on job cleanup, `destroyAllContainers()` handles everything. Pulled images remain in containerd's store for future jobs.

## Dependencies

- **containerd**: embedded, provides image pull and container lifecycle.
- **CNI plugins**: bridge + host-local + portmap, extracted at build time. Provide networking for sibling containers.
- **buildah** *(planned)*: embedded or downloaded at build time. Statically linked binary, ~30MB. Required for `docker build`.
- **No Docker daemon**: the entire point.
- **No additional capabilities**: sibling containers get the same restricted capability set as job containers.

## Implementation Order

1. ~~Socket server + health endpoints (`/_ping`, `/version`)~~ — done
2. ~~Image pull (`POST /images/create`)~~ — done
3. ~~Container create/start/stop/wait/inspect/delete~~ — done
4. ~~CNI networking for sibling containers~~ — done
5. **Exec (create/start/inspect)** — next, required for act step execution
6. **Copy to/from container** — required for act to inject step scripts
7. **Image inspect** (`GET /images/{name}/json`) — act checks before pulling
8. **Network create/connect** — required for `services:` support
9. **Image build** (`POST /build`) — embed buildah, wire up build context streaming
10. **Image push** — registry auth + `buildah push`
11. **`/etc/hosts` injection** — service discovery for named sidecars
