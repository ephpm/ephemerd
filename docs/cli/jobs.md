---
title: jobs
weight: 9
---

List and manage running jobs. When called without a subcommand, lists all active jobs. When called with a job ID as an argument, shows detailed information about that job.

```
ephemerd jobs [job-id]
ephemerd jobs <subcommand> <job-id>
```

## List jobs

Running `ephemerd jobs` with no arguments lists all active jobs.

```bash
$ ephemerd jobs
JOB ID         NAME                                     REPO                      STATUS     UPTIME
12345678       build                                    myorg/myrepo              running    5m32s
12345679       test                                     myorg/myrepo              running    3m10s
```

If no jobs are running, prints "No running jobs."

## Inspect a job

Pass a job ID as an argument to get detailed JSON output.

```bash
$ ephemerd jobs 12345678
{
  "id": 12345678,
  "name": "build",
  "repo": "myorg/myrepo",
  "image": "ghcr.io/actions/actions-runner:latest",
  "runner_id": 42,
  "status": "running",
  "pid": 9876,
  "started_at": "2025-01-15T10:30:00Z",
  "uptime": "5m32s"
}
```

## Subcommands

### kill

Force-kill a running job and deregister its runner.

```
ephemerd jobs kill <job-id>
```

Sends a `KillJob` request to the daemon via gRPC. The daemon destroys the container, tears down networking, and deregisters the GitHub runner.

### logs

Show the log output for a running job.

```
ephemerd jobs logs <job-id>
```

Streams the job's log data from the daemon via the `GetJobLogs` gRPC call. The output is written to stdout and the command exits when the log stream ends.

### ssh (macOS only)

Open an interactive SSH session to a running macOS VM job. Only works for macOS VM jobs — Linux container jobs don't have SSH access.

```
ephemerd jobs ssh <job-id>
```

The command:

1. Connects to the daemon's HTTP unix socket at `<data-dir>/ephemerd.sock.http`.
2. Retrieves the VM's IP address and ephemeral SSH private key from the `/vm/ssh-info` endpoint.
3. Opens an SSH connection to the VM on port 22 using the ephemeral key.
4. Allocates a PTY with `xterm-256color` and starts an interactive shell.
5. Handles terminal window resize events (`SIGWINCH`).

The SSH key is generated in-memory when the daemon starts and rotates on each restart. No password authentication is used.
