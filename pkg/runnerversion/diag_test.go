package runnerversion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanDiag(t *testing.T) {
	dir := t.TempDir()
	// No _diag at all: no hit, no error (dispatched runners keep it in the guest).
	if _, ok, err := ScanDiag(dir); ok || err != nil {
		t.Fatalf("no diag: %v %v", ok, err)
	}
	diag := filepath.Join(dir, "_diag")
	if err := os.MkdirAll(diag, 0o755); err != nil {
		t.Fatal(err)
	}
	older := "[2026-11-09 10:00:00Z INFO MessageListener] Runner connected.\n"
	newer := strings.Repeat("[2026-11-10 10:00:00Z INFO Listener] noise line\n", 20) +
		"[2026-11-10 10:00:05Z ERR  MessageListener] Runner version v2.337.0 is deprecated and cannot receive messages.\n" +
		"[2026-11-10 10:00:05Z INFO Runner] exiting\n"
	if err := os.WriteFile(filepath.Join(diag, "Runner_20261109-100000-utc.log"), []byte(older), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diag, "Runner_20261110-100000-utc.log"), []byte(newer), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diag, "Worker_20261110-100000-utc.log"), []byte("deprecated and cannot receive messages"), 0o644); err != nil {
		t.Fatal(err)
	}
	hit, ok, err := ScanDiag(dir)
	if err != nil || !ok {
		t.Fatalf("expected hit: %v %v", ok, err)
	}
	if !strings.HasSuffix(hit.File, "Runner_20261110-100000-utc.log") || !strings.Contains(hit.Line, "v2.337.0 is deprecated") {
		t.Fatalf("hit %+v", hit)
	}

	// Only the newest log has the line removed: older log has none either.
	if err := os.WriteFile(filepath.Join(diag, "Runner_20261110-100000-utc.log"), []byte("fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ScanDiag(dir); ok {
		t.Fatal("no marker should mean no hit (Worker_ logs are not scanned)")
	}
}

func TestScanDiagTailBound(t *testing.T) {
	dir := t.TempDir()
	diag := filepath.Join(dir, "_diag")
	if err := os.MkdirAll(diag, 0o755); err != nil {
		t.Fatal(err)
	}
	// Marker buried before the tail window must not be found; one inside must.
	pad := strings.Repeat("x", 1000) + "\n"
	var b strings.Builder
	b.WriteString("Runner version v1 is deprecated and cannot receive messages.\n")
	for b.Len() < diagTailBytes+4096 {
		b.WriteString(pad)
	}
	if err := os.WriteFile(filepath.Join(diag, "Runner_a.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ScanDiag(dir); ok || err != nil {
		t.Fatalf("marker outside tail window: %v %v", ok, err)
	}
	b.WriteString("Runner version v2 is deprecated and cannot receive messages.\n")
	if err := os.WriteFile(filepath.Join(diag, "Runner_a.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	hit, ok, err := ScanDiag(dir)
	if err != nil || !ok || !strings.Contains(hit.Line, "v2") {
		t.Fatalf("marker inside tail window: %v %v %+v", ok, err, hit)
	}
}

func TestMatchesDeprecation(t *testing.T) {
	if !MatchesDeprecation("Runner version v2.337.0 is DEPRECATED and cannot receive messages.") {
		t.Fatal("case-insensitive match")
	}
	if MatchesDeprecation("Runner connected to GitHub") {
		t.Fatal("false positive")
	}
}
