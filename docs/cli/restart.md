---
title: restart
weight: 5
---

Restart the ephemerd system service. Like `start` and `stop`, this wraps the platform's native service manager so the same command works on Linux, macOS, and Windows. Runs `stop` followed by `start`. If the stop fails (e.g., the service was not running), it prints a note and proceeds with start.

```
ephemerd restart
```

## Platform behavior

| Platform | Command |
|----------|---------|
| Linux | `systemctl restart ephemerd` |
| macOS | `launchctl unload` then `launchctl load -w /Library/LaunchDaemons/dev.ephpm.ephemerd.plist` |
| Windows | `sc.exe stop ephemerd` then `sc.exe start ephemerd` |
