# sccache for Rust compilation caching — evaluation

**Status:** design note, no implementation. Written alongside the `[cargo_proxy]`
pull-through cache (`pkg/proxies/cargo`).

**Bottom line:** worth building, but **after** the disk-pressure GC lands, and
only with a hard size cap wired into that GC from day one. A compile cache is
the single biggest available win on Rust CI wall-clock — and, if left
unbounded, the most likely next disk-fill incident.

---

## Why consider it at all

The `[cargo_proxy]` this note accompanies removes *download* cost: the sparse
index, `.crate` tarballs, and rustup toolchains stop crossing the network on
every build. That is a real saving, but it is not where Rust CI time goes.

A cold Rust CI build spends a small minority of its wall-clock fetching crates
and the overwhelming majority in `rustc`. Dependencies are the bulk of it: a
project with a few hundred transitive crates compiles all of them before it
compiles a line of its own code, and it does so again on every fresh runner
because ephemerd's containers are ephemeral by design. `cargo build` alone
caches nothing across jobs — `target/` dies with the container.

sccache addresses exactly that: it wraps `rustc`, hashes the compilation
inputs, and returns a cached object file on a hit. Unlike the registry proxy,
its benefit scales with dependency *compile* cost rather than dependency
*download* size, which is the dominant term.

## Backend choice: local disk vs shared

**Local disk (`SCCACHE_DIR`) on a host volume.** Simplest by far — a directory
on the runner host, bind-mounted into each job container. Hits come only from
jobs that previously ran *on this host*, which for a small fleet is most of
them. It has a built-in size cap (`SCCACHE_CACHE_SIZE`) with LRU eviction,
which is the single most important property given our history. No new network
service, no credentials, no cross-host trust boundary.

**Shared/remote (S3, Redis, memcached, or `sccache --start-server` over
HTTP).** Hits are fleet-wide, so a cold host benefits from a warm one, and a
scale-out fleet converges on one cache instead of N. The costs are real
though: object storage or a Redis to operate, credentials to distribute into
job containers (a secret that untrusted CI code can read), and a write path
that lets any job poison the cache for every other host. It also reintroduces
network cost on the read path, partially undoing the thing we are optimising.

**Recommendation: start local-disk-only.** It captures most of the benefit at a
fraction of the operational and security cost, and it is the configuration
whose eviction story we can actually enforce. Treat a shared backend as a
later, opt-in addition once there is evidence that cross-host misses matter.

## Integration with ephemerd's job containers

The mechanism is already built — `[cargo_proxy]` needed the same two levers,
and both generalise:

- **Env vars** via `proxies.CacheProxy.EnvVars()`. sccache is configured
  entirely through the environment: `RUSTC_WRAPPER=sccache`, `SCCACHE_DIR`,
  `SCCACHE_CACHE_SIZE`. Unlike Cargo's source replacement, this needs no
  config file, so no `MountProvider` is required for configuration.
- **A bind mount** via `proxies.MountProvider` for the cache directory itself
  — but read-**write**, which is a materially different proposition from the
  Cargo config mount (read-only). See the trust caveat below.

The awkward part is the binary. `RUSTC_WRAPPER=sccache` requires an `sccache`
executable on `PATH` inside the container, and stock runner images do not have
one. Options, in increasing order of intrusiveness:

1. Bind-mount a host-side `sccache` binary into the container and put its
   directory on `PATH` (or set `RUSTC_WRAPPER` to the absolute mounted path,
   avoiding the `PATH` edit entirely). Needs a statically-linked binary
   matching the container's libc — musl builds exist and are the obvious
   choice. This is the cheapest option and mirrors how the GitHub Actions
   runner itself is already mounted in.
2. Ship it in ephemerd's embedded assets like the runner and CNI plugins, then
   mount as above. Same runtime shape, adds a download to `mage download`.
3. Require the image to provide it. Rejected: it forces a workflow/image
   change, which the `[cargo_proxy]` design explicitly avoided.

Option 1 or 2. Note this makes the feature Linux-container-first; Windows and
the macOS native path would need separate handling and should be out of scope
for a first cut.

## Correctness caveats

sccache is conservative by design, but the failure mode of a compile cache is
*wrong output*, not a slow build, so this deserves care:

- **Hash inputs.** sccache keys on the preprocessed source, the compiler
  binary's hash, the full argument list, and the relevant env vars. That is
  sound for ordinary `rustc` invocations.
- **Proc macros and build scripts are not cached.** `build.rs` output and
  proc-macro expansion are outside sccache's model; crates that lean on them
  see less benefit. This is a benefit ceiling, not a correctness risk.
- **Incremental compilation is incompatible.** sccache refuses to cache when
  `CARGO_INCREMENTAL=1`. CI builds should set `CARGO_INCREMENTAL=0` anyway;
  ephemerd should inject it alongside `RUSTC_WRAPPER` so the two settings can
  never disagree.
- **Absolute paths leak into debug info.** Cached objects embed the paths they
  were compiled from, so per-job workdir names (`<data>/jobs/<runner-id>/…`)
  reduce the hit rate and can put a stale path in a backtrace. Mitigated with
  `--remap-path-prefix`, which is worth injecting from the start rather than
  retrofitting.
- **Toolchain churn invalidates everything.** A nightly bump changes the
  compiler hash and orphans the entire cache. With a size cap and LRU this
  self-heals; without one it silently doubles the footprint.
- **Trust boundary.** The cache directory is written by untrusted CI code and
  read by the *next* job on the host. A malicious job can plant an object
  under a key it predicts and have a later job link it. This is the same
  category of risk the existing per-repo dind image cache manages by
  namespacing per (provider, repo). The compile cache should do the same:
  **partition the cache directory per repo**, not one shared pool. That
  reduces the hit rate across repos and is the right trade.

## Disk footprint — the part that must not be skipped

We have just had two disk-exhaustion outages, both from caches that grew
without an eviction policy (most recently ~44 GB of BuildKit build cache in the
shared `buildkit` containerd namespace: 76 image records, 481 leases, 302
snapshots, never cleaned). A compile cache is exactly the same shape of risk —
it is *designed* to accumulate — so it must not ship with the same gap.

Non-negotiables for any implementation:

- **A hard size cap, configured and enforced.** `SCCACHE_CACHE_SIZE` with
  sccache's own LRU eviction, defaulting to something modest (10 GB is a
  reasonable starting point) and surfaced as a config key, not a constant.
  sccache enforces this itself, which makes it strictly better behaved than
  BuildKit's cache has been.
- **Registration in `managedCaches()`** in `cmd/ephemerd/cache.go`, so
  `ephemerd cache list` shows its size and `ephemerd cache clear sccache`
  works. It is `LiveSafe: true` — a running job that loses a cache entry
  simply recompiles. Follow the `cargo`/`gomod` entries as the template.
- **Participation in the disk-pressure GC** currently being built (which now
  also covers the `buildkit` namespace). The GC must be able to evict from the
  compile cache under pressure, and the compile cache must be low in the
  eviction priority order — below anything that would force a re-download, and
  well below live job state, but above nothing at all. A cache the GC cannot
  see is a cache that will eventually fill the disk.
- **Per-repo partitioning interacts with the cap.** N repos × a per-repo cap is
  the real ceiling. Either cap the aggregate and let the GC arbitrate, or size
  the per-repo cap knowing the multiplier. Do not set a per-repo cap and quote
  it as the total.

## Interaction with BuildKit layer caching

These two caches overlap and can double-count, which is worth designing around
rather than discovering later.

BuildKit caches at *layer* granularity: a `docker build` whose Dockerfile runs
`cargo build` produces a layer keyed on the build context and the preceding
layers. sccache caches at *compilation unit* granularity, inside that build.
When both are active for the same work you can store the same compiled output
twice — once as object files in the sccache dir, once inside a BuildKit
snapshot — and the BuildKit copy is the one that has already caused an
incident.

Two clean positions:

- **Jobs that compile Rust directly on the runner** (`cargo build` as a
  workflow step, which is the ephpm case): sccache applies, BuildKit is not
  involved. No overlap. This is the case worth optimising for.
- **Jobs that compile Rust inside `docker build`**: BuildKit's layer cache
  already covers a clean rebuild, and threading sccache into the builder means
  plumbing the cache into the build (a cache mount) plus the env vars. The
  incremental benefit over a working BuildKit cache is small.

**Recommendation: scope sccache to the direct-`cargo build` path and do not
wire it into `docker build`.** That keeps the two caches disjoint by
construction, avoids two overlapping unbounded stores, and targets the case
that actually dominates. If BuildKit-side Rust caching is wanted later, the
right lever is a BuildKit cache mount, not a second copy of sccache.

## Recommendation

Build it, with these conditions:

1. **Sequence it after the disk-pressure GC.** The GC is the prerequisite, not
   a follow-up. Shipping an unbounded compile cache into a fleet that has just
   had two disk outages would be repeating the mistake.
2. **Local disk backend, per-repo partitioned, hard size cap, registered in
   `managedCaches()` and in the GC's eviction order.**
3. **Inject `RUSTC_WRAPPER`, `SCCACHE_DIR`, `SCCACHE_CACHE_SIZE`,
   `CARGO_INCREMENTAL=0`, and `--remap-path-prefix` together**, via the
   existing `CacheProxy`/`MountProvider` plumbing. Reuse it; do not invent a
   parallel mechanism.
4. **Scope to the direct `cargo build` path**, Linux containers first.
5. **Fail open**, exactly as `[cargo_proxy]` does: if the binary or the cache
   dir is unavailable, inject nothing and let the build compile normally.
   sccache's own `SCCACHE_ERROR_LOG` should be surfaced, but a cache failure
   must never fail a job.

Expected payoff is a large reduction in Rust CI wall-clock on warm hosts —
substantially bigger than the registry proxy's, because it removes compile
time rather than download time. The risk is entirely in the disk dimension,
and it is fully mitigable with a cap the tool already implements.
