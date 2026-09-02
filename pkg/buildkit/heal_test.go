package buildkit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseDanglingSnapshot(t *testing.T) {
	tests := []struct {
		name    string
		msg     string
		wantID  string
		wantKnd SnapshotKind
		wantOK  bool
	}{
		{
			// Signature 1 from #149: a BuildKit cache record ID, the
			// build-cache half. Seen identically across three release runs.
			name:    "build cache record",
			msg:     "failed to solve: failed to prepare pfxhx2gh7f7v4tjm0lp7gd6bo as ntwv8p4dcg: parent snapshot jfvdzwv6tyfkgcimx9uaifsfh does not exist: not found",
			wantID:  "jfvdzwv6tyfkgcimx9uaifsfh",
			wantKnd: SnapshotCacheRecord,
			wantOK:  true,
		},
		{
			// Signature 2 from #149: a layer chain ID, the image-layer
			// half, surfaced once --no-cache stepped past signature 1.
			name:    "image layer chain id",
			msg:     "failed to solve: parent snapshot sha256:66462cc862fe2053b9863fefa3866e07bb5dfb06f6b3ce3177cc096e4021aabe does not exist: not found",
			wantID:  "sha256:66462cc862fe2053b9863fefa3866e07bb5dfb06f6b3ce3177cc096e4021aabe",
			wantKnd: SnapshotChainID,
			wantOK:  true,
		},
		{
			name:    "without the parent qualifier",
			msg:     "snapshot abc123def456 does not exist: not found",
			wantID:  "abc123def456",
			wantKnd: SnapshotCacheRecord,
			wantOK:  true,
		},
		{
			// A wrong positive here throws away the node's build cache, so
			// the near misses matter as much as the hits.
			name:   "missing content is a different failure",
			msg:    `failed to solve: content digest sha256:abc: not found`,
			wantOK: false,
		},
		{
			name:   "missing image is a different failure",
			msg:    "failed to solve: debian:bookworm: not found",
			wantOK: false,
		},
		{
			name:   "disk full is a different failure",
			msg:    "failed to solve: write /var/lib/ephemerd: no space left on device",
			wantOK: false,
		},
		{
			name:   "empty",
			msg:    "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseDanglingSnapshot(tt.msg)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}
			if got.Kind != tt.wantKnd {
				t.Errorf("Kind = %v, want %v", got.Kind, tt.wantKnd)
			}
		})
	}
}

func TestDanglingSnapshotFromError(t *testing.T) {
	if _, ok := DanglingSnapshotFromError(nil); ok {
		t.Fatal("nil error reported a dangling snapshot")
	}

	// The signature must survive wrapping: it arrives from the solver
	// through several layers of fmt.Errorf.
	base := errors.New("parent snapshot sha256:66462cc862fe2053b9863fefa3866e07bb5dfb06f6b3ce3177cc096e4021aabe does not exist: not found")
	wrapped := fmt.Errorf("build failed: %w", fmt.Errorf("failed to solve: %w", base))
	got, ok := DanglingSnapshotFromError(wrapped)
	if !ok {
		t.Fatal("wrapped error not recognised")
	}
	if got.Kind != SnapshotChainID {
		t.Errorf("Kind = %v, want SnapshotChainID", got.Kind)
	}
}

func TestParseDanglingLease(t *testing.T) {
	tests := []struct {
		name   string
		msg    string
		wantID string
		wantOK bool
	}{
		{
			// The #193 incident string, verbatim: a cached FROM hit a
			// cache record whose containerd lease was gone.
			name:   "missing cache record lease",
			msg:    `failed to solve: lease "4igb5uptxddpdjw9lwatd9psk": not found`,
			wantID: "4igb5uptxddpdjw9lwatd9psk",
			wantOK: true,
		},
		{
			name:   "view lease derivative",
			msg:    `failed to solve: lease "4igb5uptxddpdjw9lwatd9psk-view": not found`,
			wantID: "4igb5uptxddpdjw9lwatd9psk-view",
			wantOK: true,
		},
		{
			name:   "compression variants lease derivative",
			msg:    `lease "4igb5uptxddpdjw9lwatd9psk-variants": not found`,
			wantID: "4igb5uptxddpdjw9lwatd9psk-variants",
			wantOK: true,
		},
		{
			// A wrong positive throws away the node's build cache, so the
			// near misses matter as much as the hits. BuildKit's temporary
			// leases ("<nanos>-<base64>") are not a persistent desync — a
			// retry mints a fresh one — and must not trigger a repair.
			name:   "temporary lease id is a different failure",
			msg:    `lease "123456789-AbC_": not found`,
			wantOK: false,
		},
		{
			// containerd's other missing-lease shape carries no ID and
			// comes from a leased-context write, not a stale record.
			name:   "lease does not exist is a different failure",
			msg:    "lease does not exist: not found",
			wantOK: false,
		},
		{
			// 24 chars: not an identity.NewID (always exactly 25).
			name:   "id of the wrong length",
			msg:    `lease "4igb5uptxddpdjw9lwatd9ps": not found`,
			wantOK: false,
		},
		{
			name:   "missing content is a different failure",
			msg:    `failed to solve: content digest sha256:abc: not found`,
			wantOK: false,
		},
		{
			name:   "empty",
			msg:    "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseDanglingLease(tt.msg)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tt.wantOK, got)
			}
			if tt.wantOK && got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}
		})
	}
}

func TestDanglingLeaseFromError(t *testing.T) {
	if _, ok := DanglingLeaseFromError(nil); ok {
		t.Fatal("nil error reported a dangling lease")
	}

	// The signature must survive wrapping: it arrives from the solver
	// through several layers of fmt.Errorf.
	base := errors.New(`lease "4igb5uptxddpdjw9lwatd9psk": not found`)
	wrapped := fmt.Errorf("build failed: %w", fmt.Errorf("failed to solve: %w", base))
	got, ok := DanglingLeaseFromError(wrapped)
	if !ok {
		t.Fatal("wrapped error not recognised")
	}
	if got.ID != "4igb5uptxddpdjw9lwatd9psk" {
		t.Errorf("ID = %q", got.ID)
	}
}

func TestHealKeyFromError(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantKey string
		wantOK  bool
	}{
		{
			name:    "dangling snapshot",
			err:     errors.New("snapshot jfvdzwv6tyfkgcimx9uaifsfh does not exist: not found"),
			wantKey: "jfvdzwv6tyfkgcimx9uaifsfh",
			wantOK:  true,
		},
		{
			name:    "dangling lease",
			err:     errors.New(`failed to solve: lease "4igb5uptxddpdjw9lwatd9psk": not found`),
			wantKey: "4igb5uptxddpdjw9lwatd9psk",
			wantOK:  true,
		},
		{
			name:   "unrelated failure",
			err:    errors.New("no space left on device"),
			wantOK: false,
		},
		{
			name:   "nil",
			err:    nil,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, ok := HealKeyFromError(tt.err)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if key != tt.wantKey {
				t.Errorf("key = %q, want %q", key, tt.wantKey)
			}
		})
	}
}

func TestClassifySnapshotKey(t *testing.T) {
	tests := []struct {
		id   string
		want SnapshotKind
	}{
		{"sha256:66462cc862fe2053b9863fefa3866e07bb5dfb06f6b3ce3177cc096e4021aabe", SnapshotChainID},
		{"sha512:" + "ab12" + "0123456789abcdef0123456789abcdef", SnapshotChainID},
		{"jfvdzwv6tyfkgcimx9uaifsfh", SnapshotCacheRecord},
		// Digest-shaped but truncated: refuse to guess rather than treat a
		// chain ID as a cache record and skip the image-record eviction.
		{"sha256:deadbeef", SnapshotUnknown},
	}
	for _, tt := range tests {
		if got := classifySnapshotKey(tt.id); got != tt.want {
			t.Errorf("classifySnapshotKey(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestHealerEscalates(t *testing.T) {
	var h Healer
	d := DanglingSnapshot{ID: "sha256:abc", Kind: SnapshotChainID}

	// The ladder exists because the cheap repair does not always stick: a
	// second sighting of the SAME key means prune did not reach it.
	if got := h.Next(d); got != HealPrune {
		t.Fatalf("first = %v, want HealPrune", got)
	}
	if got := h.Next(d); got != HealRebuild {
		t.Fatalf("second = %v, want HealRebuild", got)
	}
	if got := h.Next(d); got != HealGiveUp {
		t.Fatalf("third = %v, want HealGiveUp", got)
	}
	if got := h.Next(d); got != HealGiveUp {
		t.Fatalf("fourth = %v, want HealGiveUp (must not loop back)", got)
	}

	// A different key is its own ladder — one poisoned record must not
	// escalate an unrelated one straight to a store rebuild.
	if got := h.Next(DanglingSnapshot{ID: "other", Kind: SnapshotCacheRecord}); got != HealPrune {
		t.Fatalf("unrelated key = %v, want HealPrune", got)
	}

	// After a repair sticks, the key starts over.
	h.Forget(d.ID)
	if got := h.Next(d); got != HealPrune {
		t.Fatalf("after Forget = %v, want HealPrune", got)
	}
}

func TestHealerIgnoresEmptyID(t *testing.T) {
	var h Healer
	if got := h.Next(DanglingSnapshot{}); got != HealNone {
		t.Fatalf("empty ID = %v, want HealNone", got)
	}
	if got := h.NextLease(DanglingLease{}); got != HealNone {
		t.Fatalf("empty lease ID = %v, want HealNone", got)
	}
}

func TestHealerLeaseLadder(t *testing.T) {
	// The missing-lease signature climbs the same prune → rebuild → give-up
	// ladder as the snapshot one, keyed by the lease ID.
	var h Healer
	l := DanglingLease{ID: "4igb5uptxddpdjw9lwatd9psk"}

	if got := h.NextLease(l); got != HealPrune {
		t.Fatalf("first = %v, want HealPrune", got)
	}
	if got := h.NextLease(l); got != HealRebuild {
		t.Fatalf("second = %v, want HealRebuild", got)
	}
	if got := h.NextLease(l); got != HealGiveUp {
		t.Fatalf("third = %v, want HealGiveUp", got)
	}

	// Forget re-arms the ladder after a successful retry, via the same key
	// HealKeyFromError hands the build handler.
	h.Forget(l.ID)
	if got := h.NextLease(l); got != HealPrune {
		t.Fatalf("after Forget = %v, want HealPrune", got)
	}
}

func TestHealerConcurrent(t *testing.T) {
	// Several jobs can hit the same poisoned record at once.
	var h Healer
	d := DanglingSnapshot{ID: "sha256:abc", Kind: SnapshotChainID}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Next(d)
		}()
	}
	wg.Wait()
	if got := h.Next(d); got != HealGiveUp {
		t.Fatalf("after concurrent escalation = %v, want HealGiveUp", got)
	}
}

func TestQuarantineDir(t *testing.T) {
	now := time.Unix(1786575749, 0)

	got, err := quarantineDir(filepath.Join("/var", "lib", "ephemerd", "buildkit"), now)
	if err != nil {
		t.Fatalf("quarantineDir: %v", err)
	}
	want := filepath.Join("/var", "lib", "ephemerd", "_quarantine-buildkit-1786575749-000000000")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// The quarantine must land BESIDE the store, never inside it — a fresh
	// store has to start empty.
	if filepath.Dir(got) != filepath.Dir(filepath.Join("/var", "lib", "ephemerd", "buildkit")) {
		t.Errorf("quarantine %q is not a sibling of the data dir", got)
	}

	// Renaming a filesystem root would be catastrophic; refuse instead.
	for _, bad := range []string{"/", "."} {
		if _, err := quarantineDir(bad, now); err == nil {
			t.Errorf("quarantineDir(%q) returned no error", bad)
		}
	}
}

// TestQuarantineDir_NeverCollides pins the uniquifier.
//
// The name used to be `<prefix><unix-seconds>` — one-second granularity with
// no collision handling. Two heal keys can escalate to Rebuild at the same
// moment; s.mu serializes them but does not spread them out, so both land in
// the same second and get the same path. The second Rebuild's
// os.Rename(DataDir, quarantine) then fails onto the existing directory,
// pushing a rebuild that had nothing wrong with it into
// reinitAfterFailedRebuild.
//
// The clock here is FROZEN, which is the worst case (nanoseconds cannot help)
// and the only way to test the fallback deterministically.
func TestQuarantineDir_NeverCollides(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "buildkit")
	frozen := time.Unix(1786575749, 123456789)

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		got, err := quarantineDir(dataDir, frozen)
		if err != nil {
			t.Fatalf("quarantineDir #%d: %v", i, err)
		}
		if seen[got] {
			t.Fatalf("quarantineDir returned %q twice with a frozen clock; the second Rebuild's rename would fail onto the first quarantine", got)
		}
		seen[got] = true
		// Simulate Rebuild actually moving the store there.
		if err := os.MkdirAll(got, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Nanoseconds are in the name, so an unfrozen clock does not even need
	// the suffix loop.
	a, err := quarantineDir(dataDir, time.Unix(1786575749, 1))
	if err != nil {
		t.Fatal(err)
	}
	b, err := quarantineDir(dataDir, time.Unix(1786575749, 2))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two instants inside the same second produced the same name %q", a)
	}

	// Every name must still be recognisable to pruneOldQuarantines, or a
	// node that trips this repeatedly fills its disk with evidence.
	for name := range seen {
		if !strings.HasPrefix(filepath.Base(name), quarantineDirPrefix) {
			t.Errorf("%q does not carry the quarantine prefix; pruneOldQuarantines would never reclaim it", name)
		}
	}
}

func TestPruneOldQuarantines(t *testing.T) {
	parent := t.TempDir()
	keep := filepath.Join(parent, quarantineDirPrefix+"200")
	old := filepath.Join(parent, quarantineDirPrefix+"100")
	unrelated := filepath.Join(parent, "buildkit")

	for _, d := range []string{keep, old, unrelated} {
		if err := mkdirAll(d); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	pruneOldQuarantines(parent, keep, nil)

	if !dirExists(keep) {
		t.Error("the current quarantine was removed")
	}
	if dirExists(old) {
		t.Error("a previous quarantine was retained; a node that keeps tripping this would fill its disk")
	}
	if !dirExists(unrelated) {
		t.Error("a non-quarantine directory was removed")
	}
}

func mkdirAll(p string) error { return os.MkdirAll(p, 0o700) }

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// TestMatchersRejectJobInfluencedText pins the tail anchoring and the
// near-miss shapes: the repair ladder throws away shared build cache, so it
// must be unreachable from any error string a job can influence, and from
// lease shapes that are not cache-record leases. A future "loosen the regex"
// edit must fail here, not in production.
func TestMatchersRejectJobInfluencedText(t *testing.T) {
	reject := []struct{ name, msg string }{
		// Attacker-influenced text is never the tail of the wrap chain —
		// buildkit appends its own wrapping after job output / registry
		// reason phrases. The anchor is what rejects these.
		{"registry reason phrase gets wrapped",
			`failed to solve: failed to load cache key: unexpected status from HEAD request to https://evil.example/v2/x/manifests/latest: 404 lease "aaaaaaaaaaaaaaaaaaaaaaaaa": not found (server message)`},
		{"RUN output echoed into a failure gets wrapped",
			`process "/bin/sh -c echo snapshot deadbeef does not exist: not found; false" did not complete successfully: exit code: 1`},
		// Lease shapes that are not cache-record leases.
		{"temporary lease shape", `lease "1755846719000000000-aBcD": not found`},
		{"history lease ref_ prefix", `lease "ref_4igb5uptxddpdjw9lwatd9psk": not found`},
		{"26-char id", `lease "4igb5uptxddpdjw9lwatd9pskx": not found`},
		{"24-char id", `lease "4igb5uptxddpdjw9lwatd9ps": not found`},
		{"id-less lease error", `lease does not exist: not found`},
	}
	for _, c := range reject {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := ParseDanglingLease(c.msg); ok {
				t.Fatalf("ParseDanglingLease matched %q, must reject", c.msg)
			}
			if _, ok := ParseDanglingSnapshot(c.msg); ok {
				t.Fatalf("ParseDanglingSnapshot matched %q, must reject", c.msg)
			}
		})
	}
}
