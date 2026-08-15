---
title: uncordon
weight: 9
---

Resume claiming new jobs after a cordon or an aborted drain.

```
ephemerd uncordon
```

`ephemerd drain --wait` cordons the daemon: it keeps running and keeps serving in-flight jobs, but stops claiming queued ones. If the drain times out or you interrupt it, that cordon is left in place on purpose so the node does not silently start taking work again while you are still working on it. `uncordon` clears it.

The command takes no flags. It talks to the running daemon over the control socket and prints the active job count it saw.

```bash
$ ephemerd uncordon
Uncordoned: daemon is claiming new jobs again (0 active).
```

Running jobs are unaffected -- this only changes whether new ones are claimed.

## When you need it

- A `drain --wait` hit its `--timeout` and exited non-zero, and you decided not to restart after all.
- You cordoned a node for maintenance and are done.
- An `ephemerd upgrade` aborted partway. (The daemon clears its own cordon when a restart fails to land, but this is the manual escape hatch.)

You do **not** need it after a normal restart: the cordon is runtime state on the running daemon, not config, so a restarted daemon comes up claiming jobs.

## Errors

If the daemon is not running, or the control socket is not reachable, the command fails when it tries to dial. There is nothing to uncordon in that case -- start the daemon instead.

## See also

- [drain](drain) -- cordon and wait for running jobs to finish
- [status](status) -- shows whether the daemon is draining
