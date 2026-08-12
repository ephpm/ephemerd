package buildkit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	want := filepath.Join("/var", "lib", "ephemerd", "_quarantine-buildkit-1786575749")
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
