# ephemerd jobs

List and manage currently running jobs.

## Usage

```
ephemerd jobs                List running jobs
ephemerd jobs kill <id>      Force-kill a running job
ephemerd jobs logs <id>      Stream logs from a running job
ephemerd jobs ssh <id>       SSH into a running macOS VM job
```

## Subcommands

### jobs (list)

Shows all currently running jobs with their ID, repo, image, and duration.

### jobs kill

Force-kills a running job by ID. The container is destroyed immediately without waiting for the runner to finish gracefully. The runner is deregistered from GitHub.

### jobs logs

Streams container logs from a running job. Logs are read from `<data-dir>/logs/<job-id>.log`.

### jobs ssh

Opens an interactive SSH session to a running macOS VM job. This is useful for debugging — you get a shell inside the VM where the job is executing.

```
sudo ephemerd jobs ssh 71765854232
```

The command retrieves the VM's IP address and the ephemeral SSH key from the running daemon (via a unix socket), then opens an SSH session with full PTY support. No SSH keys are stored on disk — the key exists only in the daemon's memory and rotates on every restart.

Only works for macOS VM jobs (not Linux container jobs).

## How it works

`list`, `kill`, and `logs` connect to the daemon's gRPC control socket (`<data-dir>/ephemerd.sock`) and call the `ListJobs`, `KillJob`, or `GetJobLogs` RPCs.

`ssh` connects to a separate HTTP unix socket (`<data-dir>/ephemerd.sock.http`) to retrieve the VM's IP and the ephemeral SSH private key, then opens a direct SSH connection to the VM using Go's `x/crypto/ssh` library.
