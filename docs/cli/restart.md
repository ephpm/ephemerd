---
title: restart
weight: 5
---

Restart the ephemerd system service. Like `start` and `stop`, this wraps the platform's native service manager so the same command works on Linux, macOS, and Windows.

```
ephemerd restart
```

## Platform behavior

| Platform | Behavior |
|----------|----------|
| Linux | `systemctl restart ephemerd` |
| macOS | `launchctl unload` then `launchctl load -w /Library/LaunchDaemons/dev.ephpm.ephemerd.plist`. `unload` is synchronous, so the sequential unload/load is race-free. If the unload fails (e.g. the service was not running) it prints a note and proceeds with the load |
| Windows | Drives the Service Control Manager directly: asks the SCM to stop the service, **waits for it to reach STOPPED**, then starts it. A plain `sc.exe stop` followed immediately by `sc.exe start` races the stop and can fail; the wait is what makes the restart reliable |

## Notes

- `restart` does **not** wait for running jobs. The daemon's own shutdown path finishes in-flight jobs on the way down (bounded by `shutdown_timeout`), but if you want the node idle before restarting, drain first:

  ```bash
  ephemerd drain --wait && ephemerd restart
  ```

- To move to a new release, use [`upgrade`](upgrade) rather than restarting by hand -- it drains, swaps the binary, and restarts as one operation.
