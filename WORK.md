# Multi-Provider Migration

## Goal

Support multiple CI providers simultaneously on a single ephemerd instance. A user should be able to configure `[github]` AND `[forgejo]` and have ephemerd pick up jobs from both.

## Current State

The scheduler is hardcoded to `*github.Client`. The `providers.Provider` interface exists and is well-designed, but the scheduler hasn't been migrated to use it. This migration has two phases:

1. **Phase 1: Migrate scheduler to Provider interface** (prerequisite)
2. **Phase 2: Support multiple providers concurrently**

---

## Phase 1: Scheduler → Provider Interface

### What to change

**`pkg/scheduler/scheduler.go`**

The `Config` struct has:
```go
type Config struct {
    GitHub *github.Client
    // ...
}
```

Change to:
```go
type Config struct {
    Provider providers.Provider
    // ...
}
```

There are **18 call sites** that use `s.cfg.GitHub.*` directly. Each needs to become a `s.cfg.Provider.*` call:

| Current call | Provider method | Locations |
|---|---|---|
| `s.cfg.GitHub.PollJobs()` | `provider.Start()` returns `<-chan JobEvent` | `startPolling()` |
| `s.cfg.GitHub.RegisterJITRunner()` | `provider.ClaimJob()` | `handleQueued()`, `handleLinuxJob()` |
| `s.cfg.GitHub.RemoveRunner()` | `provider.ReleaseJob()` | `handleCompleted()`, `destroyAll()` |
| `s.cfg.GitHub.FetchWorkflowImage()` | `provider.FetchJobImage()` | `handleQueued()` |
| `s.cfg.GitHub.WebhookHandler()` | `provider.(Webhook).WebhookHandler()` | webhook setup in `serve()` |
| `s.cfg.GitHub.RegisterWebhook()` | `provider.(Webhook).RegisterWebhooks()` | webhook setup |
| `s.cfg.GitHub.DeregisterWebhook()` | `provider.(Webhook).DeregisterWebhooks()` | shutdown |

**Event type**: Replace `github.JobEvent` with `providers.JobEvent` throughout the scheduler. The `providers.JobEvent` type already exists in `pkg/providers/provider.go`.

**`cmd/ephemerd/main.go`**

The `serve()` function currently constructs a `github.Client` and passes it to the scheduler. Change to:
1. Read `cfg.Provider()` to determine which provider
2. Construct the appropriate `providers.Provider` implementation
3. Pass it to the scheduler

```go
// Before:
gh, err := github.NewClient(...)
sched := scheduler.New(scheduler.Config{GitHub: gh, ...})

// After:
p, err := initProvider(cfg)  // returns providers.Provider
sched := scheduler.New(scheduler.Config{Provider: p, ...})
```

Write an `initProvider(cfg)` function that switches on `cfg.Provider()` and constructs the right implementation.

### Tests to update

- `pkg/scheduler/*_test.go` — mock the `Provider` interface instead of `*github.Client`
- `test/e2e/github/` — should still work, just wired differently

---

## Phase 2: Multiple Providers

### Config changes (`pkg/config/config.go`)

Replace `Provider() string` (returns one) with `Providers() []string` (returns all configured). Keep the existing per-section structs — just return all that have credentials.

```go
func (c *Config) Providers() []string {
    var ps []string
    if c.Forgejo.InstanceURL != "" { ps = append(ps, "forgejo") }
    if c.Gitea.InstanceURL != "" { ps = append(ps, "gitea") }
    if c.GitLab.InstanceURL != "" { ps = append(ps, "gitlab") }
    if c.Woodpecker.ServerURL != "" { ps = append(ps, "woodpecker") }
    if c.GitHub.Owner != "" || c.GitHub.Token != "" { ps = append(ps, "github") }
    return ps
}
```

Validation should check ALL configured providers, not just the first one.

### Composite job key

**Critical**: Different providers can return the same int64 job ID. The scheduler maps MUST use a composite key:

```go
type jobKey struct {
    Provider string
    JobID    int64
}
```

Change these maps in `scheduler.go`:
- `running map[int64]*runningJob` → `running map[jobKey]*runningJob`
- `seen map[int64]time.Time` → `seen map[jobKey]time.Time`

The `runningJob` struct should also store which `Provider` claimed it, so `ReleaseJob` and `RemoveRunner` call the right provider on cleanup.

### Event fan-in

Each provider's `Start()` returns its own `<-chan providers.JobEvent`. Merge them:

```go
merged := make(chan providers.JobEvent)
for _, p := range activeProviders {
    ch, _ := p.Start(ctx, pollCfg)
    go func(c <-chan providers.JobEvent) {
        for ev := range c {
            merged <- ev
        }
    }(ch)
}
```

The `JobEvent` must carry a reference to its provider (or provider name) so the scheduler knows which provider to call `ClaimJob`/`ReleaseJob` on.

Add a `Provider providers.Provider` field to `providers.JobEvent`:

```go
type JobEvent struct {
    Provider Provider  // which provider sent this event
    // ... existing fields
}
```

### Webhook multiplexing

Only GitHub implements `Webhook` today. For multi-provider webhooks, use per-provider paths:

```
/webhook/github
/webhook/forgejo
```

The tunnel URL stays singular — all paths go through the same tunnel. Each webhook-capable provider registers its own path.

### Runner naming

Current format: `ephemerd-{repo}-{random}`

Add provider prefix to avoid collisions when same repo name exists on multiple forges:
```
ephemerd-github-{repo}-{random}
ephemerd-forgejo-{repo}-{random}
```

### gRPC control API (`api/v1/ephemerd.proto`)

Add `provider` field to the `Job` message:

```protobuf
message Job {
    string provider = 6;  // "github", "forgejo", etc.
    // ... existing fields
}
```

Update `ListJobs`, `KillJob`, `GetJobLogs` to include the provider in responses.

### WSL dispatch

The `CreateJobRequest` proto currently has `jit_config` (GitHub-specific). For Forgejo/Gitea it needs env vars instead. Extend the proto:

```protobuf
message CreateJobRequest {
    string id = 1;
    string image = 2;
    string jit_config = 3;           // GitHub JIT runner config
    map<string, string> env = 4;     // provider-specific env vars
    string provider = 5;             // which provider type
}
```

The dispatch server in WSL uses the `provider` field to decide how to configure the container.

### Metrics

Add `provider` label to all job metrics:

```go
metrics.JobsTotal.WithLabelValues(provider, event.Repo, conclusion)
```

### Shutdown

`destroyAll()` must call `ReleaseJob` on each job's specific provider, not a single global provider. The `runningJob` struct already needs a `Provider` field (from the composite key work) — use it.

---

## Order of Operations

1. Migrate scheduler to `providers.Provider` interface (Phase 1)
2. Add composite job key (`jobKey` struct)
3. Add `Provider` field to `JobEvent`
4. Event fan-in (merge N channels)
5. Update `Config.Providers()` and validation
6. Update `serve()` to init and start multiple providers
7. Update gRPC proto and control API
8. Update WSL dispatch proto
9. Webhook path multiplexing
10. Runner naming with provider prefix
11. Metrics labels
12. Tests — unit tests for scheduler with mock multi-provider, e2e with GitHub + Forgejo

## Files to touch

- `pkg/scheduler/scheduler.go` — the bulk of the work
- `pkg/providers/provider.go` — add `Provider` field to `JobEvent`
- `pkg/config/config.go` — `Providers()` method, validation loop
- `cmd/ephemerd/main.go` — `initProvider()` / `initProviders()`, multi-provider wiring
- `api/v1/ephemerd.proto` — `provider` field on `Job`, dispatch env map
- `pkg/scheduler/dispatch.go` — extended `CreateJobRequest`
- `pkg/metrics/metrics.go` — provider label dimension
- `*_test.go` files across scheduler, config, e2e

## Estimated size

- Phase 1: ~300-400 lines changed
- Phase 2: ~800-1200 lines changed
- Total: ~1100-1600 lines, medium-to-large refactor
