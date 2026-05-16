---
title: Fake Docker Daemon
weight: 6
---

ephemerd provides a fake Docker Engine API server that translates Docker CLI calls into containerd operations. This allows CI jobs to run `docker build`, `docker run`, and `docker push` without a real Docker daemon or privileged containers.

## Problem

CI jobs frequently call `docker build`, `docker run`, and `docker push`. GitHub's hosted runners have a real Docker daemon available. Self-hosted runners behind ephemerd do not -- containers run with restricted capabilities (no `CAP_SYS_ADMIN`) and there is no Docker daemon inside.

Users hit this when:

- Workflows call `docker build` to produce container images.
- Workflows use `services:` to spin up databases/caches as sidecars.
- Workflows call `docker run` to test images they just built.

## Architecture

```
+------------------------------+
|  Job Container               |
|                              |
|  docker build -t myapp .     |
|       |                      |
|       v                      |
|  /var/run/docker.sock        |
+----------+-------------------+
           | Unix socket
           v
+------------------------------+
|  ephemerd (fake Docker API)  |
|                              |
|  POST /build -> buildah bud  |
|  POST /containers/create     |
|     -> containerd            |
|  POST /images/create         |
|     -> containerd pull       |
+----------+-------------------+
           |
           v
+------------------------------+
|  containerd (embedded)       |
|                              |
|  Pulls remote images         |
|  Creates sibling containers  |
|  Same network, same firewall |
+------------------------------+
```

Each job gets its own fake daemon instance (`pkg/dind/dind.go`). The daemon maintains an in-memory image store and a temp directory for OCI layers. All state is scoped to the job and destroyed when the job exits.

## API Translation

### Implemented Endpoints

| Docker API | ephemerd action | Status |
|---|---|---|
| `GET /_ping` | Returns `OK` with API version 1.45 headers | Done |
| `GET /version` | Returns `27.0.0-ephemerd` version info | Done |
| `GET /info` | Returns minimal system info with image count | Done |
| `GET /images/json` | Lists images from the in-memory store | Done |
| `POST /images/create` | Pulls images via containerd, stores in memory map | Done |

### Planned Endpoints

| Docker API | ephemerd action | Status |
|---|---|---|
| `POST /containers/create` | Create sibling container via containerd | Not yet |
| `POST /containers/{id}/start` | Start containerd task | Not yet |
| `POST /containers/{id}/exec` | Exec process in containerd task | Not yet |
| `POST /containers/{id}/stop` | Kill containerd task | Not yet |
| `DELETE /containers/{id}` | Destroy container via containerd | Not yet |
| `POST /build` | Stream build context, run `buildah bud` | Not yet |
| `POST /images/{name}/push` | Push via `buildah push` | Not yet |

### Not Supported

| Docker API | Reason |
|---|---|
| `docker compose` | Compose is a client-side tool making many API calls. Basic `docker compose up` may work if it only uses `create` + `start`, but no guarantees. |
| `docker network create` | Jobs share the ephemerd CNI network. Custom networks are not supported. |
| `docker volume create` | Use bind mounts from the job's workspace instead. |
| Swarm / Kubernetes APIs | Not applicable. |

## Sibling Containers

Sidecars created via `docker run` are **sibling containers** -- they run alongside the job container, not inside it. They share the same CNI network and firewall rules. From the job's perspective, sidecars are reachable at their container IP.

This is important: there is no nested Docker. The fake daemon creates first-class containerd containers that happen to be managed through a Docker API shim.

## Socket Lifecycle

1. **Job starts**: ephemerd creates a Unix socket at `<DataDir>/jobs/<JobID>/docker/d.sock`, starts the fake daemon goroutine, mounts the socket into the container at `/var/run/docker.sock`.
2. **Job runs**: Docker CLI in the container talks to the socket. ephemerd handles requests and creates sibling containers as sibling containers.
3. **Job finishes**: ephemerd destroys all sibling containers created by this job, deletes the temp directory, closes the socket, and runs the per-job namespace cleanup described below.

## Per-Job Namespace and Cleanup

Every job that uses dind gets its own containerd namespace:

```
ephemerd-dind-<runner-name>      e.g. ephemerd-dind-ephemerd-github-ephpm-fast_shannon
```

All sibling containers, image records, leases, and snapshots created by the job live in this namespace. When the job exits, `Server.Stop()` runs `CleanupJobNamespace`:

1. Kill and delete any in-flight tasks, delete every container with `WithSnapshotCleanup`.
2. Delete every Image record (drops the `containerd.io/gc.ref.content.*` labels that pin manifest + config + layer blobs).
3. Delete every lease.
4. Walk the snapshotter and remove snapshots **leaf-first in a multi-pass loop**. Image layer snapshots form a parent-child tree (each layer is a child of the one below) and containerd refuses to delete a snapshot that still has children. Each pass removes whatever currently has no children; the loop terminates when the snapshotter is empty or no pass makes progress.
5. Walk the content store and explicitly delete blobs (containerd's async content GC won't have swept yet by the time we want to delete the namespace).
6. `NamespaceService().Delete()` the metadata bucket itself.

A short retry loop catches transient `FailedPrecondition` errors caused by containerd's eventually-consistent state. If a snapshot is genuinely stuck, the failure is logged with the snapshot's name, parent, and kind so operators can investigate.

On worker-mode startup, `CleanupStaleDindNamespaces` sweeps everything matching `ephemerd-dind-*` that's not a cache namespace (see below), catching ungraceful exits — `DeadlineExceeded`, `SIGKILL`, host reboot — that bypassed `Server.Stop`.

## Per-Repo Image Cache

The cleanup above releases the `gc.ref` labels that previously pinned image content (manifest, config, layer blobs). Without further action, every job would pay a full network re-pull for `kindest/node` (~1 GB) and any other image the job touches.

To avoid that tax, dind maintains a **per-(provider, repo)** long-lived cache namespace:

```
ephemerd-dind-cache-<provider>-<sanitized-repo>

ephemerd-dind-cache-github-ephpm_ephpm
ephemerd-dind-cache-gitea-ephpm_ephpm        ← distinct from the github one
ephemerd-dind-cache-gitlab-acme_platform_api ← nested GitLab groups OK
```

`Provider` and `Repo` flow through `CreateJobRequest` → `runtime.CreateConfig` → `dind.Config`, so the cache namespace is derived from the dispatching forge rather than parsed from the runner name (which loses provider info).

### Cache writes

Two events mirror image metadata into the cache:

1. **Image pull (`POST /images/create`)** — after a successful pull, the Image record is created/updated in the cache namespace with an `ephemerd.io/last-accessed` label set to the current RFC3339 UTC time.
2. **Container create (`POST /containers/create`)** — if the requested image is already present in the cache (no pull needed), the cache record's `last-accessed` label is refreshed. Captures cache hits driven by `docker run` of a previously-pulled image.

The cache record's `gc.ref.content.*` labels pin the underlying content blobs in containerd's content store. Even when the per-job namespace is deleted and its Image record gone, the cache record keeps the blobs alive. The next job in the same repo gets a content-store hit and pulls only the manifest (to revalidate the digest).

### Privacy boundary

Containerd's namespace isolation is the privacy guarantee. A content blob whose only Image record reference lives in `ephemerd-dind-cache-foo-private` is **invisible** to a resolver running in any other namespace — containerd's content store lookup is namespace-scoped at the metadata layer. Two forges with same-named repos (`github/ephpm` vs `gitea/ephpm`) get distinct cache namespaces; two repos within the same forge get distinct caches keyed by the full `owner/repo` path. Auth credentials live in the per-job in-memory auth cache and are never copied into the cache namespace.

This relies on never setting the `containerd.io/namespace.shareable` label on cache namespaces. Don't.

### Cache pruning

A goroutine started in worker-mode walks every `ephemerd-dind-cache-*` namespace on a fixed interval and evicts Image records whose `last-accessed` label is older than the configured threshold. Configuration:

```toml
[dind]
  cache_prune_interval = "24h"   # how often the sweeper wakes up
  cache_max_age        = "168h"  # 7 days — LRU threshold
```

After eviction, containerd's content GC reclaims any blob no longer referenced by an Image record in any namespace. Cache namespaces left empty after a prune pass are removed entirely so unused-repo metadata doesn't accumulate.

Image records pre-dating the `last-accessed` label fall back to the record's `UpdatedAt` timestamp on first prune, so introducing this feature doesn't nuke pre-existing caches.

## Enabling

Enable with `dind.enabled = true` in config or the `--dind` flag on `serve`:

```toml
[dind]
  enabled = true
  cache_prune_interval = "24h"
  cache_max_age        = "168h"
```

## Key Files

| File | Purpose |
|------|---------|
| `pkg/dind/dind.go` | Fake Docker API server, route dispatch, image pull, cache-mirror on pull |
| `pkg/dind/containers.go` | Container lifecycle, `last-accessed` refresh on container-create |
| `pkg/dind/cleanup.go` | Per-job namespace cleanup (containers, images, leases, snapshots leaf-first, content, namespace) + boot-time stale sweep |
| `pkg/dind/cache.go` | Per-repo cache namespace name derivation + sanitization, mirror helper, last-accessed refresh, periodic prune |
| `pkg/dind/dind_test.go` | Tests for health and image endpoints |
| `pkg/dind/cleanup_test.go` | Tests covering full namespace teardown + stale-sweep prefix filter |
| `pkg/dind/cache_test.go` | Tests covering cross-provider isolation, sanitization invariants, mirror + refresh + prune lifecycle |
