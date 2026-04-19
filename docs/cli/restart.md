---
title: restart
weight: 5
---

Restart the ephemerd system service. Like `start` and `stop`, this wraps the platform's native service manager so the same command works on Linux, macOS, and Windows. Runs `stop` followed by `start`. If the stop fails (e.g., the service was not running), it prints a note and proceeds with start.

```
ephemerd restart
```
