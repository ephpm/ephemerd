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
2. **Job runs**: Docker CLI in the container talks to the socket. ephemerd handles requests and creates sidecars as sibling containers.
3. **Job finishes**: ephemerd destroys all sibling containers created by this job, deletes the temp directory, closes the socket. No leaked state.

## Enabling

Enable with `dind.enabled = true` in config or the `--dind` flag on `serve`:

```toml
[dind]
enabled = true
```

## Key Files

| File | Purpose |
|------|---------|
| `pkg/dind/dind.go` | Fake Docker API server, route dispatch, image pull |
| `pkg/dind/dind_test.go` | Tests for health and image endpoints |
