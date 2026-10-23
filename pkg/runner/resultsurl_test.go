package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func fakeDLL(parts ...[]byte) []byte {
	var b []byte
	b = append(b, 0x4d, 0x5a, 0x90, 0x00) // "MZ" header noise
	for _, p := range parts {
		b = append(b, 0x27) // #US heap length-prefix noise
		b = append(b, p...)
		b = append(b, 0x00)
	}
	return b
}

func TestPatchResultsURL_Found(t *testing.T) {
	data := fakeDLL(utf16LE("ACTIONS_RUNTIME_TOKEN"), utf16LE(ResultsURLVar), utf16LE("ACTIONS_CACHE_URL"))
	size := len(data)
	if !PatchResultsURL(data) {
		t.Fatal("PatchResultsURL = false, want true for an assembly holding the string")
	}
	if len(data) != size {
		t.Fatalf("patch changed the length: %d -> %d", size, len(data))
	}
	if bytes.Contains(data, utf16LE(ResultsURLVar)) {
		t.Error("original string still present after patch")
	}
	if !bytes.Contains(data, utf16LE(ResultsURLRenamed)) {
		t.Error("renamed string missing after patch")
	}
	// Neighbours are untouched.
	for _, keep := range []string{"ACTIONS_RUNTIME_TOKEN", "ACTIONS_CACHE_URL"} {
		if !bytes.Contains(data, utf16LE(keep)) {
			t.Errorf("patch damaged neighbouring string %q", keep)
		}
	}
}

func TestPatchResultsURL_NotFound(t *testing.T) {
	data := fakeDLL(utf16LE("SOMETHING_ELSE"))
	// An ASCII copy is not what the CLR reads; it must not count.
	data = append(data, []byte(ResultsURLVar)...)
	before := append([]byte(nil), data...)
	if PatchResultsURL(data) {
		t.Fatal("PatchResultsURL = true for an assembly without the UTF-16 string")
	}
	if !bytes.Equal(before, data) {
		t.Error("PatchResultsURL modified data it did not patch")
	}
}

func TestPatchResultsURL_AlreadyPatched(t *testing.T) {
	data := fakeDLL(utf16LE(ResultsURLRenamed))
	if !PatchResultsURL(data) {
		t.Fatal("an already-patched assembly must report true (idempotent across restarts)")
	}
}

func TestRenamedVarSameLength(t *testing.T) {
	if len(ResultsURLVar) != len(ResultsURLRenamed) {
		t.Fatalf("%q and %q differ in length; the #US heap entry is length-prefixed", ResultsURLVar, ResultsURLRenamed)
	}
}

func writeDLL(t *testing.T, root string, data []byte) string {
	t.Helper()
	p := filepath.Join(root, WorkerDLL)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPatchResultsURLFile_WritesToWritableRootOnly(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	orig := fakeDLL(utf16LE(ResultsURLVar))
	lowerPath := writeDLL(t, lower, orig)

	ok, err := PatchResultsURLFile(lower, upper)
	if err != nil || !ok {
		t.Fatalf("PatchResultsURLFile = %v, %v; want true, nil", ok, err)
	}
	got, _ := os.ReadFile(lowerPath)
	if !bytes.Equal(got, orig) {
		t.Fatal("the shared runner tree was modified; only the writable copy may change")
	}
	patched, err := os.ReadFile(filepath.Join(upper, WorkerDLL))
	if err != nil {
		t.Fatalf("no patched copy in the writable root: %v", err)
	}
	if !bytes.Contains(patched, utf16LE(ResultsURLRenamed)) {
		t.Error("writable copy is not patched")
	}

	// Second call (daemon restart, same job dir) stays true.
	if ok, err := PatchResultsURLFile(lower, upper); err != nil || !ok {
		t.Fatalf("second PatchResultsURLFile = %v, %v; want true, nil", ok, err)
	}
}

func TestPatchResultsURLFile_InPlace(t *testing.T) {
	root := t.TempDir()
	p := writeDLL(t, root, fakeDLL(utf16LE(ResultsURLVar)))
	if ok, err := PatchResultsURLFile(root, ""); err != nil || !ok {
		t.Fatalf("PatchResultsURLFile = %v, %v; want true, nil", ok, err)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Contains(got, utf16LE(ResultsURLRenamed)) {
		t.Error("in-place patch did not land")
	}
}

// TestPatchResultsURL_RealRunner runs the patch against a real
// Runner.Worker.dll when EPHEMERD_RUNNER_WORKER_DLL points at one. Run it on
// every runner bump: a new runner that stops carrying the string would
// silently turn the LAN cache off.
//
//	EPHEMERD_RUNNER_WORKER_DLL=/path/to/bin/Runner.Worker.dll go test ./pkg/runner -run RealRunner -v
func TestPatchResultsURL_RealRunner(t *testing.T) {
	path := os.Getenv("EPHEMERD_RUNNER_WORKER_DLL")
	if path == "" {
		t.Skip("set EPHEMERD_RUNNER_WORKER_DLL to a Runner.Worker.dll to check a real runner")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, utf16LE(ResultsURLVar)); n != 1 {
		t.Errorf("%s holds %d UTF-16 copies of %s; the patch was verified against exactly 1", path, n, ResultsURLVar)
	}
	size := len(data)
	if !PatchResultsURL(data) {
		t.Fatal("real runner was not patched")
	}
	if len(data) != size || bytes.Contains(data, utf16LE(ResultsURLVar)) {
		t.Fatal("patched assembly changed size or still holds the original name")
	}
	t.Logf("patched %s (%d bytes)", path, size)
}

func TestPatchResultsURLFile_MissingAssembly(t *testing.T) {
	ok, err := PatchResultsURLFile(t.TempDir(), t.TempDir())
	if ok || err != nil {
		t.Fatalf("missing assembly = %v, %v; want false, nil (cache stays off, extraction is not failed)", ok, err)
	}
}
