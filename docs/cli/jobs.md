# ephemerd jobs

List and manage currently running jobs.

## Usage

```
ephemerd jobs                List running jobs
ephemerd jobs kill <id>      Force-kill a running job
ephemerd jobs logs <id>      Stream logs from a running job
```

## Subcommands

### jobs (list)

Shows all currently running jobs with their ID, repo, image, and duration.

### jobs kill

Force-kills a running job by ID. The container is destroyed immediately without waiting for the runner to finish gracefully. The runner is deregistered from GitHub.

### jobs logs

Streams container logs from a running job. Use `--follow` to tail logs in real-time (like `tail -f`). Logs are read from `<data-dir>/logs/<job-id>.log`.

## How it works

Connects to the daemon's gRPC control socket and calls the `ListJobs`, `KillJob`, or `GetJobLogs` RPCs.
