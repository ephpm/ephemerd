package config

import "time"

// BuildKitConfig bounds the embedded BuildKit solver's on-disk build cache.
//
// WHY THIS TABLE EXISTS: BuildKit only garbage-collects when its worker is
// given a GC policy, and ephemerd never gave it one. Every `docker build`
// run by every CI job therefore added cache records, snapshots and
// `containerd.io/gc.flat` leases to the shared "buildkit" containerd
// namespace that nothing ever released. A production node accumulated
// 76 image records, 302 snapshots and 481 leases — about 44 GB of a 116 GB
// disk — spanning 49 dead jobs over two and a half weeks, which is what
// actually filled the disk and froze the VM.
//
// The defaults aim for a WARM BUT BOUNDED cache. Pruning hard on every
// build would trade the disk problem for a network problem (re-downloading
// and re-building layers on every job), which is the opposite of what these
// nodes need. So there is a floor that is never collected, a ceiling that
// always is, and a free-space guard that overrides the floor when the node
// is genuinely tight.
type BuildKitConfig struct {
	// GCEnabled toggles BuildKit's cache garbage collection. Nil = default
	// true. Setting false restores the old unbounded behavior and is only
	// sensible while debugging a cache-correctness problem.
	GCEnabled *bool `toml:"gc_enabled"`

	// GCReservedGB is build cache, in GiB, that is never collected even
	// when idle. The warm floor. Default 5.
	GCReservedGB uint64 `toml:"gc_reserved_gb"`

	// GCMaxUsedGB is the hard ceiling, in GiB, on total build cache.
	// Anything above it is collected regardless of age. Default 25.
	GCMaxUsedGB uint64 `toml:"gc_max_used_gb"`

	// GCMinFreeGB makes BuildKit collect whatever it must to keep at least
	// this much free space, in GiB, on the filesystem — overriding
	// GCReservedGB. This is the arm that rescues a node whose disk is
	// being consumed by something other than the build cache. Default 20,
	// matching [image_gc].min_free_gb so the two collectors agree on when
	// the node is tight.
	GCMinFreeGB uint64 `toml:"gc_min_free_gb"`

	// GCKeepDuration is the age past which cache records are collected
	// once usage exceeds GCReservedGB. Default 168h (7 days). BuildKit's
	// own default is 60 days, which is far too long for a CI node that
	// rebuilds the same images many times a day.
	GCKeepDuration time.Duration `toml:"gc_keep_duration"`

	// GCEphemeralKeepDuration and GCEphemeralMaxUsedGB bound the cheaply
	// reproducible record types (local build contexts, RUN --mount=cache
	// mounts, git checkouts). Re-creating those costs a local copy rather
	// than a network round trip, so they are collected far more eagerly.
	// Defaults 48h and 2 GiB.
	GCEphemeralKeepDuration time.Duration `toml:"gc_ephemeral_keep_duration"`
	GCEphemeralMaxUsedGB    uint64        `toml:"gc_ephemeral_max_used_gb"`
}

const gibibyte = 1024 * 1024 * 1024

// BuildKitGCEnabled reports whether the build cache is bounded. Default true.
func (b *BuildKitConfig) BuildKitGCEnabled() bool {
	if b.GCEnabled != nil {
		return *b.GCEnabled
	}
	return true
}

// BuildKitGCReservedBytes returns the never-collected floor, default 5 GiB.
func (b *BuildKitConfig) BuildKitGCReservedBytes() int64 {
	return gbOrDefault(b.GCReservedGB, 5)
}

// BuildKitGCMaxUsedBytes returns the hard ceiling, default 25 GiB.
func (b *BuildKitConfig) BuildKitGCMaxUsedBytes() int64 {
	return gbOrDefault(b.GCMaxUsedGB, 25)
}

// BuildKitGCMinFreeBytes returns the free-space guard, default 20 GiB.
func (b *BuildKitConfig) BuildKitGCMinFreeBytes() int64 {
	return gbOrDefault(b.GCMinFreeGB, 20)
}

// BuildKitGCKeepDuration returns the record age limit, default 7 days.
// A negative value disables the age arm (size bounds still apply).
func (b *BuildKitConfig) BuildKitGCKeepDuration() time.Duration {
	if b.GCKeepDuration < 0 {
		return 0
	}
	if b.GCKeepDuration == 0 {
		return 7 * 24 * time.Hour
	}
	return b.GCKeepDuration
}

// BuildKitGCEphemeralKeepDuration returns the ephemeral-record age limit,
// default 48h.
func (b *BuildKitConfig) BuildKitGCEphemeralKeepDuration() time.Duration {
	if b.GCEphemeralKeepDuration < 0 {
		return 0
	}
	if b.GCEphemeralKeepDuration == 0 {
		return 48 * time.Hour
	}
	return b.GCEphemeralKeepDuration
}

// BuildKitGCEphemeralMaxUsedBytes returns the ephemeral-record ceiling,
// default 2 GiB.
func (b *BuildKitConfig) BuildKitGCEphemeralMaxUsedBytes() int64 {
	return gbOrDefault(b.GCEphemeralMaxUsedGB, 2)
}

// gbOrDefault converts a GiB count to bytes, substituting def when unset.
// Sizes large enough to overflow int64 are impossible for a disk bound, so
// the conversion is unchecked.
func gbOrDefault(gb, def uint64) int64 {
	if gb == 0 {
		gb = def
	}
	return int64(gb) * gibibyte
}
