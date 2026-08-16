---
title: Registry Cache
weight: 6
---

A pull-through registry cache on the LAN is the single largest bandwidth saving available to an ephemerd node. This guide covers what to run, what to point ephemerd at, and how to confirm it is working.

The configuration reference for the keys used here is [`[registry_mirror]`](../getting-started/configuration.md#registry_mirror).

## Why

Every job pulls its image. On an ephemerd node most of those pulls are the *same* image, and most of them cross the WAN.

Measured on a production `linux-amd64` node:

| Measurement | Value |
|---|---|
| Inbound traffic | 294 GB over 4.1 days (~71 GB/day) |
| Jobs served | ~80/day |
| Inbound per job | ~890 MB |
| Job image `ephpm/ephpm-ci:latest` | 1,099 MB |
| Pulls of that one image | 163 in 7 days (~14-23/day) |

The image is pulled repeatedly because dind pulls into a **per-job containerd namespace**: containerd's content store and snapshotter are both namespaced, so an image resolved in one job's namespace cannot be reused from another's. Each job therefore re-resolves and re-fetches the same manifest and layers.

With a cache on the LAN, only the first pull of a given layer crosses the WAN. Every later pull — from any job, on any node pointed at the same cache — is served at local link speed. Two secondary benefits come free: the node stops consuming Docker Hub's anonymous rate limit (only the cache talks to Hub), and a Hub outage stops being a job-failure source for images the cache already holds.

**The benefit scales with sharing.** It is largest when many jobs pull the same large base image, which is the normal shape of a CI fleet. A pool where every job pulls a different small image will see very little.

## Running a cache

### registry:2 in proxy mode

The upstream `registry:2` image has a built-in pull-through mode and is the simplest thing that works. One instance proxies exactly one upstream registry.

```bash
docker run -d --restart=always \
  --name registry-cache \
  -p 5000:5000 \
  -v /srv/registry-cache:/var/lib/registry \
  -e REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io \
  registry:2
```

Notes:

- **Storage.** `/srv/registry-cache` grows to roughly the working set of distinct layers your fleet pulls. Budget for a few multiples of your largest base image; a handful of CI images typically lands in the tens of gigabytes. `registry:2` has no automatic eviction — schedule `registry garbage-collect` or size the volume generously.
- **Upstream credentials.** For private Docker Hub repositories, give the *cache* the credentials (`REGISTRY_PROXY_USERNAME` / `REGISTRY_PROXY_PASSWORD`); ephemerd does not need to forward any. This also lifts the rate limit for the whole fleet with a single authenticated identity.
- **One upstream per instance.** To also cache `ghcr.io`, run a second instance on another port and map it with `[registry_mirror.mirrors]`.
- **Plain HTTP is fine on a trusted LAN** and is what the `http://` endpoint form is for. ephemerd does not send registry credentials to the mirror unless you explicitly enable `forward_credentials`, precisely so a plaintext endpoint cannot harvest them.

### Alternatives

- **[Zot](https://zotregistry.dev/)** — a small OCI-native registry with pull-through sync, including scheduled and on-demand mirroring of multiple upstreams from one instance. A good fit when you want one endpoint in front of Docker Hub *and* ghcr.io.
- **[Harbor](https://goharbor.io/)** — a full registry platform whose *proxy cache* projects do the same job, plus RBAC, quotas, replication and scanning. Each proxy project is published under a path (`https://harbor.lan/v2/dockerhub-proxy`); ephemerd accepts a path prefix on `endpoint` and joins `/v2` behind it. Worth it if you already run Harbor, heavy otherwise.

Any registry that implements the OCI distribution pull API in a proxying mode will work — ephemerd speaks standard `/v2` to it.

## Pointing ephemerd at it

```toml
[registry_mirror]
enabled  = true
endpoint = "http://registry.lan:5000"
```

That is the whole minimum configuration. It mirrors `docker.io` (the default for `registries`) and keeps the origin registry behind the cache.

To add a second cache for another registry:

```toml
[registry_mirror]
enabled  = true
endpoint = "http://registry.lan:5000"

[registry_mirror.mirrors]
"ghcr.io" = "http://ghcr-cache.lan:5000"
```

Restart ephemerd. On startup it logs the resolved policy once:

```
level=INFO msg="registry mirror active" component=registry-mirror \
  mirrors=docker.io=http://registry.lan:5000 fallback_to_origin=true forward_credentials=false
```

## What happens when the cache is down

Nothing, from the job's point of view.

ephemerd hands containerd the ordered host list `[mirror, origin]`. containerd's resolver tries each in turn and moves to the next whenever one fails to connect or answers 4xx/5xx. A cache that is stopped, wedged, out of disk, or simply does not hold the requested image costs one failed request; the pull then completes against the origin registry at the WAN speed the node had before the mirror existed. **The job still runs.**

There is deliberately no health check, no circuit breaker and no state: the fallback is containerd's own retry loop, which is exercised on every pull and cannot drift out of sync with reality.

The one way to change this is `fallback_to_origin = false`, which drops the origin from the list entirely. That is an egress-control decision — jobs can then only run images the cache holds — and not a performance setting. Do not set it to work around a flaky cache.

## Verifying it is working

1. **ephemerd's log.** Each mirrored pull logs the endpoint it is going to first:

   ```
   level=INFO msg="pulling through registry mirror" component=registry-mirror \
     ref=docker.io/ephpm/ephpm-ci:latest mirror=http://registry.lan:5000 fallback_to_origin=true
   ```

2. **The cache's own log.** `docker logs registry-cache` shows the `/v2/.../manifests/...` and blob requests arriving. First pull of a layer shows an upstream fetch; later pulls are served locally.

3. **Node inbound traffic.** The real confirmation. Compare bytes in over a day of similar job volume before and after. On a fleet whose jobs share one large base image, the expected shape is inbound-per-job dropping toward the size of the *job-specific* content only.

4. **Rate-limit headers.** If Docker Hub throttling was a symptom, it should disappear from job logs entirely — only the cache talks to Hub now.

## Credentials

By default the mirror is contacted **anonymously**, even when a job has run `docker login`. Only the origin registry receives credentials.

This is deliberate. A pull-through cache normally carries its own upstream credentials and needs none from the client, and a mirror that responded with a `Basic` challenge would otherwise collect the registry PAT the job just logged in with — in cleartext when the endpoint is `http://`.

Set `forward_credentials = true` only for a mirror you operate that requires authentication (Harbor with a robot account, Zot behind htpasswd). Prefer `https://` when you do.

Pushes are never affected: `docker push` from a job always goes to the origin registry. The mirror is advertised to containerd with pull and resolve capabilities only.

## Windows and macOS hosts

Linux jobs on Windows and macOS hosts run inside a Linux VM, and it is the *in-VM* containerd that performs the pull. Where the mirror configuration has to reach therefore depends on the host:

- **Linux hosts** — pulls happen on the host. Nothing extra.
- **Windows hosts** — the host's `config.toml` is staged into the Hyper-V Linux VM's boot initrd on every VM start and passed to the in-VM daemon as `--config` (see [host config delivery](../arch/host-config-initrd.md)). A `[registry_mirror]` block set on the host therefore takes effect inside the VM on the next restart, with no extra steps. Windows *container* jobs pull on the host itself and are covered directly.
- **macOS hosts** — the Vz Linux VM does **not** yet receive the host `config.toml`; its init script boots the in-VM daemon with kernel-cmdline settings only. A mirror configured on a macOS host currently applies to host-side pulls (macOS VM job artifact extraction) but not to Linux jobs dispatched into the VM. The host data directory is already shared into that VM over virtio-fs, so the fix is to pass `--config` pointing at the shared `config.toml` from the darwin init script — the same mechanism Windows uses, and tracked separately.

The cache itself must be reachable from wherever the pull happens. On Windows and macOS that is the VM, which sits behind the host's VM network — confirm the cache address is routable from inside it before enabling.

## Interaction with the other image caches

The registry mirror is a *network* cache and is independent of the on-disk caches ephemerd already keeps:

- **Per-repo dind cache** (`[dind]`) keeps pulled images in a long-lived containerd namespace per (provider, repo), so a repeat job in the same repo gets a content-store hit and makes no request at all. The mirror is what covers the case that cache misses — a new repo, a cold node, or content the cache did not retain.
- **[`[image_gc]`](../getting-started/configuration.md#image_gc)** evicts local images under disk pressure. Every eviction is a future pull; with a mirror configured, that pull is a LAN pull rather than a WAN pull, which makes aggressive local GC considerably cheaper.
