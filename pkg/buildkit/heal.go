package buildkit

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// This file implements recovery from the single most expensive failure mode
// the shared build store has: a snapshot that BuildKit's own bbolt cache
// metadata still references but containerd's snapshotter no longer has.
//
// WHY IT HAPPENS AT ALL. BuildKit keeps a SECOND source of truth about
// containerd snapshots. Its cache metadata (<dataDir>/buildkit/cache.db and
// <dataDir>/buildkit/worker/<snapshotter>/metadata_v2.db) records, per cache
// record, the containerd snapshot key that backs it — the record's own ID for
// exec layers, and for pulled layers the layer CHAIN ID, i.e. exactly the same
// key space containerd's image unpacker uses (see buildkit
// cache/manager.go: `snapshotID := chainID.String()`). BuildKit trusts that
// metadata absolutely: on the next build it hands the recorded key straight to
// Snapshotter.Prepare as the parent, with no existence check. If the snapshot
// was removed from containerd behind BuildKit's back, Prepare fails with
//
//	failed to solve: failed to prepare <id>: parent snapshot <id> does not exist: not found
//
// and it fails that way FOREVER, identically, for every subsequent job,
// because nothing in the normal path ever revisits the stale record.
//
// WHAT REMOVES A SNAPSHOT BEHIND BUILDKIT'S BACK. Anything that touches
// containerd directly rather than going through BuildKit:
//
//   - `ctr -n buildkit snapshots rm` / `ctr -n buildkit leases rm`, e.g. an
//     operator manually reclaiming disk during an incident. This is what
//     poisoned the production Linux runner in #149: ~267 snapshots and 481
//     leases were removed by hand while BuildKit's bbolt store was left
//     untouched, and every `docker build` on that node failed from then on.
//   - restoring, rolling back or wiping containerd's data dir without wiping
//     BuildKit's alongside it.
//   - a crash between BuildKit's lease deletion and its metadata clear.
//
// Note what is NOT on that list: BuildKit's own GC. Its prune pass only
// removes records with no live refs, and a child holds a ref on its parent, so
// it collects bottom-up and cannot strand a parent (cache/manager.go's
// pruneOnce, `if len(cr.refs) == 0`). Nor can containerd's own reference
// counting: BuildKit pins each snapshot with a lease, and containerd image
// records pin their layer chain through a
// `containerd.io/gc.ref.snapshot.<snapshotter>` label on the config blob.
// Reference counting is not the hole. The hole is that two databases describe
// one store and only one of them is consulted.
//
// THE FIX. Since we cannot stop out-of-band deletion from ever happening, we
// make its consequence self-limiting: recognise the signature, repair the
// shared store, and retry — so the class of failure costs one build, not every
// build until a human logs in.

// danglingSnapshotRe matches containerd's snapshot-not-found message as it
// surfaces through a BuildKit solve. Both shapes appear in the wild:
//
//	parent snapshot sha256:66462cc862fe... does not exist: not found
//	snapshot jfvdzwv6tyfkgcimx9uaifsfh does not exist: not found
//
// The ID character class allows the ':' and digest hex of a chain ID as well
// as BuildKit's base-36 record IDs.
// Tail-anchored for the same reason as danglingLeaseRe below: the genuine
// containerd error is the wrap chain's suffix ("... does not exist: not
// found" ends the string), while a RUN command that merely PRINTS a matching
// phrase gets further wrapping appended ("did not complete successfully:
// exit code ..."), so job-controlled text cannot reach the repair ladder.
var danglingSnapshotRe = regexp.MustCompile(`(?:parent )?snapshot ([A-Za-z0-9][A-Za-z0-9._:+\-]*) does not exist(?:: not found)?$`)

// SnapshotKind classifies a dangling snapshot key by who owns it, which
// decides what repairing it involves.
type SnapshotKind int

const (
	// SnapshotUnknown is a key we cannot classify.
	SnapshotUnknown SnapshotKind = iota

	// SnapshotChainID is an image layer chain ID ("sha256:..."). This key
	// space is SHARED: containerd's image unpacker and BuildKit's
	// blob-backed cache records both address layers by chain ID. A dangling
	// chain ID therefore usually means one or more containerd image records
	// in the shared namespace are still resolvable while the layers they
	// name are gone — those records must be evicted too, or the next pull
	// resolves to the same broken chain.
	SnapshotChainID

	// SnapshotCacheRecord is a BuildKit cache record ID — a build-cache
	// layer produced by a RUN step. No containerd image record can
	// reference it, so repairing it is purely a BuildKit-side concern.
	SnapshotCacheRecord
)

func (k SnapshotKind) String() string {
	switch k {
	case SnapshotChainID:
		return "chain-id"
	case SnapshotCacheRecord:
		return "cache-record"
	default:
		return "unknown"
	}
}

// DanglingSnapshot identifies a snapshot BuildKit believes in and containerd
// does not.
type DanglingSnapshot struct {
	// ID is the containerd snapshot key from the error message.
	ID string
	// Kind is what that key addresses. See SnapshotKind.
	Kind SnapshotKind
}

// ParseDanglingSnapshot extracts the offending snapshot key from an error
// message, reporting false when the message is some other failure. Pure — it
// is the whole detection rule, so the signature we act on is testable without
// standing up BuildKit.
//
// Deliberately narrow. Auto-repair throws away the node's shared build cache;
// firing it on a merely similar message (a missing *content* blob, a
// permission error, a full disk) would turn an unrelated build failure into a
// cache wipe. Only "snapshot X does not exist" qualifies.
func ParseDanglingSnapshot(msg string) (DanglingSnapshot, bool) {
	m := danglingSnapshotRe.FindStringSubmatch(msg)
	if m == nil {
		return DanglingSnapshot{}, false
	}
	id := strings.TrimSuffix(m[1], ":")
	if id == "" {
		return DanglingSnapshot{}, false
	}
	return DanglingSnapshot{ID: id, Kind: classifySnapshotKey(id)}, true
}

// DanglingSnapshotFromError is ParseDanglingSnapshot over an error value.
// Nil errors report false.
func DanglingSnapshotFromError(err error) (DanglingSnapshot, bool) {
	if err == nil {
		return DanglingSnapshot{}, false
	}
	return ParseDanglingSnapshot(err.Error())
}

// danglingLeaseRe matches containerd's lease-not-found message as it surfaces
// through a BuildKit solve (#193):
//
//	lease "4igb5uptxddpdjw9lwatd9psk": not found
//
// This is the LEASE half of the same "two databases describe one store"
// corruption the dangling-snapshot signature covers: BuildKit's bbolt
// metadata still holds a cache record while the containerd lease that record
// owns is gone (an out-of-band `ctr leases rm`, a restored containerd data
// dir, or a crash between a lease deletion and the matching metadata clear).
// The record loads fine — nothing on the load path checks the lease — and
// detonates only when a build cache-hits it and tries to attach a resource
// to the missing lease (buildkit source/containerimage/pull.go on a cached
// FROM, cache/blobs.go setBlob on export), failing the job with the raw
// containerd error above.
//
// Deliberately narrow, for the same reason as danglingSnapshotRe: repair
// throws away build cache. Only a quoted BuildKit cache-record lease ID — 25
// base-36 characters (identity.NewID), optionally the -view / -variants
// derivative — followed by containerd's exact "not found" qualifies.
// BuildKit's temporary leases have a different shape ("<nanos>-<base64>") and
// a missing one does not indicate a persistent desync, so they do not match.
//
// Anchored to the END of the error string: a genuine containerd not-found is
// always the tail of the wrap chain, while job-influenced text (a hostile
// registry's HTTP reason phrase, a RUN command's own output echoed into a
// failure) is followed by more wrapping ("did not complete successfully",
// status codes, ...). Without the anchor, a fork PR could embed a fake
// lease line in an error it controls and trigger a shared-cache prune —
// repair must never be reachable from attacker-influenced text.
var danglingLeaseRe = regexp.MustCompile(`lease "([a-z0-9]{25}(?:-view|-variants)?)": not found$`)

// DanglingLease identifies a containerd lease BuildKit's metadata believes in
// and containerd no longer has.
type DanglingLease struct {
	// ID is the missing lease's ID from the error message — a BuildKit
	// cache record ID, since the record's lease shares its ID.
	ID string
}

// ParseDanglingLease extracts the missing lease ID from an error message,
// reporting false when the message is some other failure. Pure — the whole
// detection rule, testable without standing up BuildKit.
func ParseDanglingLease(msg string) (DanglingLease, bool) {
	m := danglingLeaseRe.FindStringSubmatch(msg)
	if m == nil {
		return DanglingLease{}, false
	}
	return DanglingLease{ID: m[1]}, true
}

// DanglingLeaseFromError is ParseDanglingLease over an error value. Nil
// errors report false.
func DanglingLeaseFromError(err error) (DanglingLease, bool) {
	if err == nil {
		return DanglingLease{}, false
	}
	return ParseDanglingLease(err.Error())
}

// HealKeyFromError returns the escalation-ladder key for err's repairable
// shared-store signature, if it has one. It is what a caller that only needs
// the Healer bookkeeping key (e.g. to Forget after a successful retry) should
// use, so it stays in sync with every signature healAndRetryBuild acts on.
func HealKeyFromError(err error) (string, bool) {
	if d, ok := DanglingSnapshotFromError(err); ok {
		return d.ID, true
	}
	if l, ok := DanglingLeaseFromError(err); ok {
		return l.ID, true
	}
	return "", false
}

// chainIDRe matches an OCI layer chain ID: an algorithm, a colon and a hex
// digest. Anything else is BuildKit's own record ID.
var chainIDRe = regexp.MustCompile(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[0-9a-fA-F]{32,}$`)

func classifySnapshotKey(id string) SnapshotKind {
	if chainIDRe.MatchString(id) {
		return SnapshotChainID
	}
	if strings.Contains(id, ":") {
		// Digest-shaped but not a valid digest — do not guess.
		return SnapshotUnknown
	}
	return SnapshotCacheRecord
}

// HealAction is what the healer wants done about a dangling snapshot.
type HealAction int

const (
	// HealNone means do nothing — this is not a repairable signature.
	HealNone HealAction = iota

	// HealPrune evicts the image records whose layer chain is broken and
	// prunes BuildKit's cache so the stale metadata record goes away. Cheap
	// and surgical enough to run inline, mid-build.
	HealPrune

	// HealRebuild discards BuildKit's cache metadata store entirely and
	// reconstructs the solver against the live containerd. This is the
	// escalation for a store whose corruption survived HealPrune: the two
	// databases disagree in a way we cannot enumerate, so the derived one
	// is rebuilt from the authoritative one.
	HealRebuild

	// HealGiveUp means repair has already been tried at both levels for
	// this key and did not stick. Fail the build loudly rather than loop.
	HealGiveUp
)

func (a HealAction) String() string {
	switch a {
	case HealPrune:
		return "prune"
	case HealRebuild:
		return "rebuild"
	case HealGiveUp:
		return "give-up"
	default:
		return "none"
	}
}

// Healer decides how hard to try when a dangling snapshot is seen, and
// remembers what it has already tried.
//
// The escalation ladder exists because the cheap repair is not always enough.
// HealPrune clears the records BuildKit is willing to enumerate; if the same
// key comes back, BuildKit's metadata is inconsistent in a way its own prune
// cannot reach (a record it refuses to size because the snapshot behind it is
// missing, for instance) and only rebuilding the derived store fixes it.
//
// State is per-daemon and in-memory on purpose. A restart re-arms the ladder,
// which is correct: after a restart the store may be a different store.
//
// The zero Healer is ready to use. Safe for concurrent use — several jobs can
// hit the same poisoned record at once.
type Healer struct {
	mu   sync.Mutex
	seen map[string]HealAction
}

// Next reports the action to take for d and records it, so a second sighting
// of the same key escalates instead of repeating a repair that did not work.
func (h *Healer) Next(d DanglingSnapshot) HealAction {
	return h.nextID(d.ID)
}

// NextLease is Next for the missing-lease signature. Lease IDs and snapshot
// keys share one ladder map: they cannot collide (a lease ID is a full
// 25-char record ID, a repairable snapshot key is a chain ID or record ID
// with different provenance), and even a collision would only escalate a
// repair one rung early.
func (h *Healer) NextLease(l DanglingLease) HealAction {
	return h.nextID(l.ID)
}

func (h *Healer) nextID(id string) HealAction {
	if id == "" {
		return HealNone
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen == nil {
		h.seen = map[string]HealAction{}
	}
	next := HealPrune
	switch h.seen[id] {
	case HealPrune:
		next = HealRebuild
	case HealRebuild, HealGiveUp:
		next = HealGiveUp
	}
	h.seen[id] = next
	return next
}

// Forget drops the remembered escalation state for a key. Called once a build
// succeeds after a repair, so a key that recurs weeks later starts from the
// cheap rung again rather than jumping straight to a rebuild.
func (h *Healer) Forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.seen, id)
}

// HealReport summarises one repair attempt, for logging and for the message
// surfaced to the job when repair fails.
type HealReport struct {
	// Snapshot is the key that triggered the repair, when the trigger was
	// the dangling-snapshot signature.
	Snapshot DanglingSnapshot
	// Lease is the key that triggered the repair, when the trigger was the
	// missing-lease signature. Exactly one of Snapshot and Lease is set.
	Lease DanglingLease
	// Action is the rung of the ladder that was executed.
	Action HealAction
	// ImagesEvicted counts containerd image records dropped because their
	// layer chain resolved to the missing snapshot.
	ImagesEvicted int
	// BytesReleased is what BuildKit's prune reported freeing.
	BytesReleased int64
	// Rebuilt reports that the BuildKit metadata store was quarantined and
	// the solver reconstructed.
	Rebuilt bool
}

// String renders the report for a log line or an error message.
func (r HealReport) String() string {
	if r.Lease.ID != "" {
		return fmt.Sprintf("lease=%s action=%s bytes_released=%d rebuilt=%t",
			r.Lease.ID, r.Action, r.BytesReleased, r.Rebuilt)
	}
	return fmt.Sprintf("snapshot=%s kind=%s action=%s images_evicted=%d bytes_released=%d rebuilt=%t",
		r.Snapshot.ID, r.Snapshot.Kind, r.Action, r.ImagesEvicted, r.BytesReleased, r.Rebuilt)
}
