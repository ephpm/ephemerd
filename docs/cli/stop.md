---
title: stop
weight: 4
---

Stop the ephemerd system service. Like `start` and `restart`, this wraps the platform's native service manager so you don't need to remember the OS-specific command.

```
ephemerd stop
```

## Platform behavior

| Platform | Command |
|----------|---------|
| Linux | `systemctl stop ephemerd` |
| macOS | `launchctl unload /Library/LaunchDaemons/dev.ephpm.ephemerd.plist` |
| Windows | `sc.exe stop ephemerd` |
