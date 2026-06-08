# Per-Container Resource Metrics

> **Status: implemented.** First landed alongside this doc. The
> `ephemerd_container_*` metric series are emitted from the Windows
> host's `/metrics` endpoint for both native Hyper-V Windows
> containers and Linux containers running inside the embedded
> Hyper-V VM. Scrape config and a reference Grafana dashboard live
> in `examples/observability/`.

## Context

ephemerd already exposes a Prometheus endpoint (`pkg/metrics/`) with
daemon-level series — `ephemerd_jobs_total`, `_jobs_active`,
`_job_duration_seconds`, GitHub API stats — plus the containerd builtins.
What it did not expose was anything about the resources a *running job
container* was consuming: CPU usage, memory in use, the configured
limits, whether jobs were bumping into them.

That gap mattered because the Windows runner config in
`pkg/config/config.go` hardcodes default Hyper-V container resources at
4 GB / 2 vCPU (`MemoryBytes`/`CPUCount`), and there was no way to know
whether those caps were correct for the workload without external
tooling (Task Manager, `Get-Counter`, `hcsdiag`). For Linux containers
the situation was worse — they're not assigned any resource limits by
the runtime at all, and cgroup stats were buried inside the VM where
the Windows-side dashboards couldn't see them.

This doc covers the design decisions for closing that gap with one
unified set of `ephemerd_container_*` series scraped from a single
`/metrics` endpoint on the host.

## Goals

1. **Unified scrape target.** One `/metrics` endpoint per host
   exposing per-container resource series for *every* container
   ephemerd is responsible for, regardless of whether it runs in the
   native Windows container runtime or the embedded Linux VM.
2. **Low overhead.** Sampling cost dominated by the underlying
   stats call (containerd `Task.Metrics`, HCS `Properties`), not by
   ephemerd glue. Single shared ticker, not one goroutine per
   container.
3. **Bounded cardinality.** The `id` label is essential for
   correlating a series with a real workflow run, but unbounded job
   IDs would explode Prometheus' on-disk size. We bound the live
   series count by ruthlessly calling `DeleteLabelValues` on
   container destroy.
4. **No new protocol direction.** The in-VM ephemerd is already a
   gRPC server that the Windows host dials into for job dispatch;
   reuse that connection. Don't introduce a second listener inside
   the VM, don't add Prometheus federation, don't ship an OTel
   collector.

## Non-goals

- Macro host metrics (host CPU, host memory). Node-exporter already
  covers that.
- Per-process metrics inside the container.
- Historical storage. Prometheus' job, not ephemerd's.
- macOS VM jobs. Different stats API (Vz); deferred.
- Pre-built dashboards beyond a single reference. Each operator's
  alerting story differs.

## Metric set

All four series are labelled `{id, repo, runtime}`. `id` is the
ephemerd job ID (matches `ephemerd_jobs_total{...}` and the
per-job runner log filename); `repo` is the forge-native repo path
(`owner/repo`); `runtime` is one of:

- `windows-hyperv` — Windows host's native runtime, runs Hyper-V
  isolated Windows containers.
- `linux-vm` — embedded Hyper-V Linux VM's containerd, samples
  pushed back to the host over the Dispatch stream.
- `linux-native` — bare-metal Linux ephemerd (future; same sampler,
  no Dispatch hop).

Series:

| Name | Type | Notes |
|---|---|---|
| `ephemerd_container_cpu_usage_seconds_total` | counter | Cumulative CPU time. Use `rate()` for utilization. |
| `ephemerd_container_memory_bytes` | gauge | Current memory in use (cgroup `memory.current` / HCS working set). |
| `ephemerd_container_memory_anon_bytes` | gauge | Anonymous-only memory. Closer to "RSS"; less polluted by page cache. Linux only — Windows reports 0. |
| `ephemerd_container_memory_limit_bytes` | gauge | Configured memory cap. `0` = unlimited. |
| `ephemerd_container_cpu_limit` | gauge | Configured vCPU count. `0` = unlimited. |
| `ephemerd_container_network_rx_bytes_total` | counter | Cumulative bytes received by the container's netns / HCS endpoints. Use `rate()` for download bandwidth. |
| `ephemerd_container_network_tx_bytes_total` | counter | Cumulative bytes sent. Use `rate()` for upload bandwidth. |

The two `_limit_*` series are exposed even though they're static for
a container's lifetime — having them as series means a Grafana panel
can do `memory_bytes / memory_limit_bytes` arithmetic in promQL
without joining against a config source.

## Cardinality

Prometheus' weakness is high-cardinality labels. `id` is high
cardinality (one per job, ever). Two mitigations:

1. **Hard delete on container destroy.** When `runtime.Destroy` runs,
   the metrics registry calls `DeleteLabelValues(id, repo, runtime)`
   on every metric. Prometheus drops the series; the next scrape
   returns nothing for that id. Operationally this means the live
   series count is bounded by `max_concurrent` plus a handful of
   transient series during teardown.
2. **No PID label, no command-line label, no image-tag label.** Those
   would all add churn without analytical value.

`repo` is bounded by the number of repos a single ephemerd serves
(handful), so it's safe.

## Sampler interface

```go
// pkg/metrics/sampler.go
type Sampler interface {
    Sample(ctx context.Context) (ContainerStats, error)
}

type ContainerStats struct {
    CPUUsageNanos    uint64
    MemoryBytes      uint64
    MemoryAnonBytes  uint64
    CPULimit         uint64 // cores, 0 = unlimited
    MemoryLimitBytes uint64 // 0 = unlimited
}
```

Per-OS implementations, picked at build time:

- **Linux** (`sampler_linux.go`): wraps `containerd.Task.Metrics(ctx)`,
  unmarshals the resulting Any into `cgroupsv2.Metrics`, and reads
  `CPU.UsageUsec`, `Memory.Usage`, `Memory.Anon`. cgroup v1 is not
  supported — every kernel ephemerd ships supports v2 (the embedded
  Linux VM kernel is built v2-only). Network counters live outside
  the cgroup; the sampler reads them via `netlink.NewHandleAt` against
  the container's netns path (`/var/run/netns/...` or `/proc/<pid>/ns/net`)
  and sums `rx_bytes`/`tx_bytes` across all non-loopback interfaces.
- **Windows** (`sampler_windows.go`): opens the compute system via
  `hcsshim.OpenContainer(id)`, calls `Statistics()`. Reads
  `Processor.TotalRuntime100ns` (converted to nanoseconds),
  `Memory.UsageCommitBytes`, and sums `BytesReceived`/`BytesSent`
  across `[]NetworkStats` endpoints. Returns `MemoryAnonBytes = 0` —
  HCS doesn't split anon vs. file.
- **Other** (`sampler_other.go`): no-op stub for cross-compilation
  and tests on non-target platforms.

## Registry

```go
// pkg/metrics/container.go
func Register(id, repo, runtime string, sampler Sampler)
func Unregister(id, repo, runtime string)
func RecordSample(id, repo, runtime string, s ContainerStats)
```

`Register` adds the container to an internal map keyed by `(id, repo,
runtime)`. A single shared ticker (configurable interval, default
`10s`) iterates the map, calls `sampler.Sample`, updates the gauges
via `RecordSample`. `Unregister` removes the map entry and calls
`DeleteLabelValues` on each metric.

The Linux-VM path skips the ticker: the in-VM dispatch server runs
its own ticker, sends batches over the gRPC stream, and the host
calls `RecordSample` directly on receipt. This avoids one of three
distasteful alternatives:

- Running a Prometheus *federation* — too heavy.
- Mirroring containerd state on the host — error-prone, the VM
  already has the source of truth.
- Letting the host poll the VM with `Task.Metrics` over an
  exposed containerd TCP socket — works, but means the
  containerd-only listener has to be exposed across the hvsock
  boundary, which it currently isn't.

## Transport

The new RPC is added to the existing `Dispatch` service on
`pkg/scheduler/dispatch.go`:

```proto
rpc StreamContainerStats(StreamContainerStatsRequest)
    returns (stream ContainerStatsBatch);
```

Server-streaming, host is client, in-VM ephemerd is server. The host
opens the stream once at startup, the in-VM ephemerd ticks at the
client-requested interval (`StreamContainerStatsRequest.interval_seconds`),
batches stats for every active env, sends one `ContainerStatsBatch`
per tick. Stream stays open for the daemon's lifetime; client
reconnects on drop with backoff.

Polarity matches the existing CreateJob/WaitJob/DestroyJob calls
(host → VM) and means the VM never needs to know an IP/port for the
host. Reconnect logic lives entirely on the host side where ephemerd
already owns the VM lifecycle.

A `ContainerStatsBatch` carries `timestamp_unix_nano` so a slow
client doesn't desynchronize the counter math: the host trusts the
server's clock for the sample timestamp but uses its own wall clock
for the `rate()` denominator.

## Configuration

`pkg/config/config.go` MetricsConfig gains one knob:

```toml
[metrics]
enabled = true
port = 9090
path = "/metrics"
container_stats_interval = "10s"  # how often samplers run
```

`container_stats_interval` is a `time.Duration` string. Default `10s`.
Used both by the host's own ticker (for native containers) and as the
default the host passes to the in-VM dispatch stream. Lower values
get finer-grained data at the cost of more HCS / cgroup syscalls;
higher values save syscalls at the cost of resolution.

## Network sampling caveats

The network counters scope to the **runner container's netns**. They
include traffic the runner itself sends/receives (image pulls happening
in the runner, artifact uploads, npm/pip/composer downloads run by the
job script). They **do not** include traffic from sibling containers
the job spawns via the fake Docker socket — each dind sibling gets its
own netns and its own counters, which ephemerd does not currently
expose. For most "did my CI eat the bandwidth budget?" questions this
is what you want; for byte-perfect cluster cost attribution it's not.

Loopback is excluded so that chatty in-container processes (e.g.,
postgres + a test client both on `127.0.0.1`) don't dwarf the
external numbers.

On Linux, the netns is opened via the path stored in `RunnerEnv.Netns`
(set by the CNI plugin chain). Network sampling is silently skipped
when that field is empty — useful for hostnet-only test setups but
also means an operator who disables CNI sees no network series. The
sampler logs at debug level when the netns lookup fails (container
teardown race, permission issue) and returns zero bytes; CPU/memory
keep flowing.

## What we deliberately didn't do

- **No per-container goroutines.** The shared ticker fans out
  serially. With `max_concurrent=4` and sub-millisecond sampler
  calls, this is fine. If we ever sample 100+ containers, we move
  to a worker pool.
- **No histogram for sampling latency.** Easy to add if the ticker
  ever starts lagging; not worth the schema noise today.
- **No per-interface network stats.** We sum across all non-loopback
  interfaces rather than emitting one series per veth/endpoint —
  per-interface adds cardinality (per-device labels) without analytical
  value for CI workloads, which only ever talk through `eth0`.
- **No disk stats.** cgroup v2 `io.stat` and HCS `StorageStats` are
  both available but not yet wired; will revisit when an operator
  actually asks a disk-IO question.
- **No federation, no OTel collector.** ephemerd speaks Prometheus
  format directly; nothing in the middle.

## Testing

- Sampler interface: fakes per platform asserting the field mapping
  from raw stats payloads.
- Registry: concurrent Register/Unregister, ticker, ensures
  `DeleteLabelValues` is called on Unregister.
- Dispatch stream: in-process gRPC roundtrip — server sends N
  batches, client feeds them into a fake registry, assert exactly
  what the host would see.
- Live verification: the reference Grafana dashboard in
  `examples/observability/` exercises every metric on a real job.
