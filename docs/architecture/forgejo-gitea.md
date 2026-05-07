---
title: Forgejo and Gitea
weight: 9
---

Forgejo and Gitea both descend from the same codebase and share the `runner.v1.RunnerService` ConnectRPC protocol, but their runners have diverged. ephemerd supports both through the same two-container model.

> **Status:** Architecture design with provider stubs and e2e tests. Full integration with upstream runners in containers is pending.

## Runner Comparison

| | Forgejo | Gitea |
|---|---|---|
| Runner binary | `forgejo-runner` | `act_runner` |
| Runner image | `data.forgejo.org/forgejo/runner:12` | `docker.io/gitea/act_runner:latest` |
| Proto package | `code.forgejo.org/forgejo/actions-proto` | `code.gitea.io/actions-proto-go` |
| Ephemeral mode | `one-job --handle <uuid>` | `daemon --ephemeral` |
| Job image | `gitea/runner-images:ubuntu-24.04` | `gitea/runner-images:ubuntu-24.04` |

Both use [nektos/act](https://github.com/nektos/act) forks as the workflow execution engine. The integration model is identical for both -- only the binary, image references, and ephemeral invocation differ.

## The Two-Container Model

Unlike GitHub Actions where the runner binary lives inside the job container, Forgejo/Gitea runners operate as external daemons that create job containers via the Docker API:

```
GitHub Actions:     [ container: runner + job steps ]          (one container)
Forgejo/Gitea:      [ container: runner daemon ] --Docker API--> [ container: job steps ]   (two containers)
```

ephemerd exploits this by mounting its [fake Docker socket]({{< relref "fake-docker-daemon" >}}) (`pkg/dind`) into the runner container. When the runner daemon calls `docker run` to create a job container, the fake socket intercepts the call and translates it to containerd operations. The job container becomes a sibling managed by ephemerd -- not a nested container.

## Architecture

```mermaid
flowchart TB
    F["Forge Instance<br/>(Forgejo or Gitea)"]

    subgraph H ["ephemerd host (Linux, Windows via WSL2, or macOS via Vz)"]
        E[ephemerd]
        CTD["containerd"]
        DSock["Fake Docker Socket<br/>pkg/dind<br/>/var/run/docker.sock"]

        subgraph RC ["Runner Container"]
            direction TB
            FR["forgejo-runner / act_runner"]
            ACT["nektos/act engine"]
            FR --- ACT
        end

        subgraph JC ["Job Container<br/>gitea/runner-images:ubuntu-24.04"]
            STEPS["workflow steps<br/>(checkout, build, test, etc.)"]
        end

        subgraph SC ["Service Containers<br/>(postgres, redis, etc.)"]
            SVC["service daemons"]
        end
    end

    E -->|"1. pull runner image<br/>2. create container<br/>3. mount fake socket"| CTD
    CTD -->|"starts"| RC
    FR -->|"4. Register + FetchTask<br/>(ConnectRPC over HTTP)"| F
    F -->|"5. task payload<br/>(YAML, context, secrets)"| FR

    ACT -->|"6. docker pull job-image"| DSock
    DSock -->|"7. containerd pull"| CTD

    ACT -->|"8. docker create + start"| DSock
    DSock -->|"9. containerd create"| CTD
    CTD -->|"10. sibling container"| JC

    ACT -->|"docker exec steps"| JC
    ACT -->|"docker create services"| DSock
    CTD -->|"sibling container"| SC

    FR -->|"11. UpdateTask + UpdateLog<br/>(ConnectRPC)"| F
    FR -->|"12. exit (ephemeral)"| RC

    RC -.->|"container exits"| E
    E -->|"13. destroy runner<br/>+ all siblings"| CTD

    style RC fill:#e1f5ff,stroke:#0288d1
    style JC fill:#fff3e0,stroke:#f57c00
    style SC fill:#fff3e0,stroke:#f57c00
    style DSock fill:#f3e5f5,stroke:#7b1fa2
```

### Lifecycle

1. ephemerd creates the runner container from the upstream runner image, with the fake Docker socket bind-mounted at `/var/run/docker.sock`.
2. containerd starts the runner -- on Linux directly, inside WSL2 on Windows, inside the Vz Linux VM on macOS.
3. Runner registers with the forge as an ephemeral runner and long-polls `FetchTask`.
4. Forge returns a task -- workflow YAML bytes, context, secrets, vars.
5. act parses the workflow and determines the job image from `runs-on:` label mapping.
6. act calls `docker pull` for the job image. The fake socket translates this to a containerd pull.
7. act calls `docker create` + `docker start`. The fake socket creates a sibling containerd container.
8. act calls `docker exec` for each workflow step inside the job container.
9. Service containers (`services:` in the workflow) are created the same way -- more siblings.
10. Runner streams logs back to the forge via `UpdateLog` and reports status via `UpdateTask`.
11. Runner exits because it was ephemeral.
12. ephemerd detects the exit, destroys the runner container and all siblings.

## Fake Docker Socket Integration

```mermaid
sequenceDiagram
    participant ACT as nektos/act<br/>(inside runner container)
    participant SOCK as Fake Docker Socket<br/>pkg/dind
    participant CTD as containerd
    participant JOB as Job Container

    Note over ACT,SOCK: act thinks it's talking to Docker

    ACT->>SOCK: GET /_ping
    SOCK-->>ACT: OK (API v1.45)

    ACT->>SOCK: POST /images/create?fromImage=node&tag=20-bookworm
    SOCK->>CTD: client.Pull("node:20-bookworm")
    CTD-->>SOCK: image pulled
    SOCK-->>ACT: {"status":"Pull complete"}

    ACT->>SOCK: POST /containers/create {image, env, mounts, cmd}
    SOCK->>CTD: create container with OCI spec
    CTD-->>SOCK: container ID
    SOCK-->>ACT: {"Id":"abc123"}

    ACT->>SOCK: POST /containers/abc123/start
    SOCK->>CTD: task.Start()
    CTD-->>JOB: container running

    ACT->>SOCK: POST /containers/abc123/exec {cmd: ["bash","-c","npm test"]}
    SOCK->>CTD: task.Exec(...)
    JOB-->>CTD: output + exit code

    Note over SOCK: All containers tagged with runner ID for cleanup
```

## Runner Pool Model

ephemerd maintains a pool of N ephemeral runner containers (where N = `max_concurrent`). Each registers with the forge, handles one job, and exits. ephemerd replaces it immediately.

```mermaid
sequenceDiagram
    participant E as ephemerd
    participant C as containerd
    participant R1 as Runner 1
    participant R2 as Runner 2
    participant F as Forge

    E->>C: create runner 1 (ephemeral)
    C->>R1: start
    R1->>F: Register(ephemeral=true)

    E->>C: create runner 2 (ephemeral)
    C->>R2: start
    R2->>F: Register(ephemeral=true)

    Note over F: job A queued
    F-->>R1: FetchTask -> job A
    R1->>R1: execute (spawns job + service containers)
    R1->>F: UpdateTask(success) + UpdateLog(...)
    R1->>R1: exit

    E->>C: destroy R1 + all siblings
    E->>C: create runner 3 (replacement)

    Note over R2: still waiting for a job
```

### Pool-Based (Current)

Zero protocol code in ephemerd. The runner handles registration, polling, execution, and reporting. ephemerd just manages container lifecycle.

- Pros: simple, matches how most people deploy today.
- Cons: N idle runner containers when no jobs are queued (minimal cost -- runner images are ~18MB).

### Demand-Based (Future)

ephemerd implements a lightweight FetchTask poller to detect pending jobs, then spawns runners on demand. No idle containers. Requires the protocol client but avoids standing containers.

## Host OS Support

Forgejo/Gitea Actions is a Linux-jobs-only ecosystem today. On all three host OSes, the runner is always a Linux container:

| Host OS | How Linux containers run |
|---------|-------------------------|
| Linux | Direct containerd |
| Windows | containerd inside WSL2 |
| macOS | containerd inside Vz Linux VM |

## Configuration

```toml
# Forgejo
[forgejo]
instance_url = "https://codeberg.org"
token = "runner-registration-token"    # from admin > Actions > Runners
owner = "your-org"
# repos = ["repo1", "repo2"]          # optional, omit for all repos
# job_image = "gitea/runner-images:ubuntu-24.04"

# Gitea (mutually exclusive with [forgejo])
[gitea]
instance_url = "https://gitea.example.com"
token = "runner-registration-token"
owner = "your-org"

[runner]
max_concurrent = 4  # pool size
```

## ephemerd-runner-forgejo (implemented)

The two-container model works for Linux jobs but is a dead end for Windows and macOS — nektos/act only creates Linux Docker containers. **ephemerd-runner-forgejo** replaces it with a single-container model: a Go binary that speaks the Forgejo/Gitea ConnectRPC protocol directly, executes steps via `os/exec` process spawning (no Docker), and cross-compiles for all platforms.

See [ephemerd-runner-forgejo architecture]({{< relref "ephemerd-runner-forgejo" >}}) for the full design.
