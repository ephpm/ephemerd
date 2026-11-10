package runnerversion

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir(), testLog())
	s.goos = "linux" // archives in tests are tar.gz regardless of host
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	return s
}

func putArchive(t *testing.T, s *Store, version, content string) (string, []byte) {
	t.Helper()
	data, sha := fakeArchive(t, content)
	if _, err := s.Put(bytes.NewReader(data), Sidecar{Version: version, Platform: "linux-x64", SHA256: sha, Sources: []string{"test"}}, 1<<20); err != nil {
		t.Fatal(err)
	}
	return sha, data
}

func listBlobs(t *testing.T, s *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(s.blobsDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestStorePutVerifiesBeforeCommit(t *testing.T) {
	s := newTestStore(t)
	data, sha := fakeArchive(t, "echo hi")

	// Wrong expected hash: nothing committed, no temp left behind.
	_, err := s.Put(bytes.NewReader(data), Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: zeroSHA}, 1<<20)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
	if blobs := listBlobs(t, s); len(blobs) != 0 {
		t.Fatalf("blobs after failed put: %v", blobs)
	}

	// Over the cap: refused, nothing left.
	_, err = s.Put(bytes.NewReader(data), Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: sha}, int64(len(data))-1)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	if blobs := listBlobs(t, s); len(blobs) != 0 {
		t.Fatalf("blobs after too-large put: %v", blobs)
	}

	// Right hash: blob + sidecar, named by hash.
	p, err := s.Put(bytes.NewReader(data), Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: sha, Sources: []string{"manifest", "github"}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != sha+".tar.gz" {
		t.Fatalf("blob name %s", filepath.Base(p))
	}
	sc, err := s.Sidecar(sha)
	if err != nil || sc.Size != int64(len(data)) || len(sc.Sources) != 2 {
		t.Fatalf("sidecar: %v %+v", err, sc)
	}
	// Idempotent.
	if _, err := s.Put(bytes.NewReader(data), Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: sha}, 1<<20); err != nil {
		t.Fatal(err)
	}
}

func TestStoreExtractSwapRollbackPrune(t *testing.T) {
	s := newTestStore(t)
	if _, ok, err := s.Current(); err != nil || ok {
		t.Fatalf("fresh store should have no current: %v %v", ok, err)
	}

	shaA, _ := putArchive(t, s, "2.338.0", "echo A")
	// Swap before extract is refused.
	if err := s.Swap(shaA, "2.338.0"); !errors.Is(err, ErrNotInStore) {
		t.Fatalf("swap before extract: %v", err)
	}
	dirA, err := s.Extract(shaA, "2.338.0")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dirA, "run.sh")); err != nil || string(b) != "echo A" {
		t.Fatalf("extracted content: %v %q", err, b)
	}
	if err := s.Swap(shaA, "2.338.0"); err != nil {
		t.Fatal(err)
	}
	cur, ok, err := s.Current()
	if err != nil || !ok || cur.SHA256 != shaA || cur.Dir != dirA || cur.Version != "2.338.0" {
		t.Fatalf("current: %v %v %+v", err, ok, cur)
	}

	shaB, _ := putArchive(t, s, "2.338.2", "echo B")
	if _, err := s.Extract(shaB, "2.338.2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Swap(shaB, "2.338.2"); err != nil {
		t.Fatal(err)
	}
	cur, _, _ = s.Current()
	prev, ok, _ := s.Previous()
	if cur.SHA256 != shaB || !ok || prev.SHA256 != shaA {
		t.Fatalf("after second swap: cur=%s prev=%s", cur.SHA256, prev.SHA256)
	}

	// A third one, then prune: A must go, B and C stay.
	shaC, _ := putArchive(t, s, "2.339.0", "echo C")
	if _, err := s.Extract(shaC, "2.339.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.Swap(shaC, "2.339.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(); err != nil {
		t.Fatal(err)
	}
	blobs := strings.Join(listBlobs(t, s), ",")
	if strings.Contains(blobs, shaA) || !strings.Contains(blobs, shaB) || !strings.Contains(blobs, shaC) {
		t.Fatalf("prune kept wrong set: %s", blobs)
	}
	if _, err := os.Stat(dirA); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pruned extraction still present: %v", err)
	}

	// Rollback: B current, C previous.
	if err := s.Rollback(); err != nil {
		t.Fatal(err)
	}
	cur, _, _ = s.Current()
	prev, _, _ = s.Previous()
	if cur.SHA256 != shaB || prev.SHA256 != shaC {
		t.Fatalf("after rollback: cur=%s prev=%s", cur.SHA256, prev.SHA256)
	}
}

func TestStoreVerifyDropsCorruptBlob(t *testing.T) {
	s := newTestStore(t)
	sha, _ := putArchive(t, s, "2.338.2", "echo ok")
	if _, err := s.Extract(sha, "2.338.2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Swap(sha, "2.338.2"); err != nil {
		t.Fatal(err)
	}
	// Good blob verifies.
	res := s.Verify()
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("verify good: %+v", res)
	}
	// Corrupt it on disk: pointer, blob, extraction all go.
	if err := os.WriteFile(s.blobPath(sha, "tar.gz"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = s.Verify()
	if len(res) != 1 || res[0].OK || !errors.Is(res[0].Err, ErrChecksumMismatch) {
		t.Fatalf("verify corrupt: %+v", res)
	}
	if _, ok, _ := s.Current(); ok {
		t.Fatal("corrupt current should have been dropped")
	}
	if s.Has(sha, "tar.gz") {
		t.Fatal("corrupt blob should have been deleted")
	}
}

func TestStoreVerifyReextractsMissingDir(t *testing.T) {
	s := newTestStore(t)
	sha, _ := putArchive(t, s, "2.338.2", "echo ok")
	dir, err := s.Extract(sha, "2.338.2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Swap(sha, "2.338.2"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	res := s.Verify()
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("verify should re-extract from a good blob: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil {
		t.Fatalf("re-extraction missing: %v", err)
	}
}

func TestStoreInitRemovesTempFiles(t *testing.T) {
	s := newTestStore(t)
	stale := filepath.Join(s.blobsDir(), tmpPrefix+"crashed.tar.gz")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale temp file survived Init")
	}
}

func TestStoreLastApplied(t *testing.T) {
	s := newTestStore(t)
	got, err := s.LastApplied()
	if err != nil || !got.IsZero() {
		t.Fatalf("fresh: %v %v", got, err)
	}
	want := validManifest("2.338.2", "2.338.0", zeroSHA).PublishedAt
	if err := s.SetLastApplied(want); err != nil {
		t.Fatal(err)
	}
	got, err = s.LastApplied()
	if err != nil || !got.Equal(want) {
		t.Fatalf("round trip: %v %v", got, err)
	}
}
