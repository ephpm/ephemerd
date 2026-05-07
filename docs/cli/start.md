---
title: start
weight: 3
---

Start the ephemerd system service. Because ephemerd runs on Linux, macOS, and Windows — each with a different service manager — these commands provide a single interface so you don't need to remember `systemctl` vs `launchctl` vs `sc.exe`.

```
ephemerd start
```

## Platform behavior

| Platform | Command |
|----------|---------|
| Linux | `systemctl start ephemerd` |
| macOS | `launchctl load -w /Library/LaunchDaemons/dev.ephpm.ephemerd.plist` |
| Windows | `sc.exe start ephemerd` |
