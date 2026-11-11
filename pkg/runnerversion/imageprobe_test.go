package runnerversion

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// depsJSON is the shape of bin/Runner.Listener.deps.json as shipped in the
// 2.337.0 archives (abbreviated).
const depsJSON = `{
  "runtimeTarget": { "name": ".NETCoreApp,Version=v8.0/linux-x64", "signature": "" },
  "targets": {
    ".NETCoreApp,Version=v8.0/linux-x64": {
      "Runner.Listener/2.337.0": {
        "dependencies": { "Runner.Common": "1.0.0", "Runner.Sdk": "1.0.0" },
        "runtime": { "Runner.Listener.dll": {} }
      },
      "Runner.Common/1.0.0": {},
      "Runner.Sdk/1.0.0": {}
    }
  },
  "libraries": { "Runner.Listener/2.337.0": { "type": "project", "serviceable": false } }
}`

func TestParseListenerDepsVersion(t *testing.T) {
	v, err := ParseListenerDepsVersion([]byte(depsJSON))
	if err != nil || v != "2.337.0" {
		t.Fatalf("%v %q", err, v)
	}
	if _, err := ParseListenerDepsVersion([]byte(`{"targets":{"Runner.Common/1.0.0":{}}}`)); !errors.Is(err, ErrNoRunnerTree) {
		t.Fatalf("expected ErrNoRunnerTree, got %v", err)
	}
}

func writeRunnerTree(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"targets":{"x":{"Runner.Listener/` + version + `":{}}}}`)
	if err := os.WriteFile(filepath.Join(dir, ListenerDepsFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProbeRunnerTree(t *testing.T) {
	dir := t.TempDir()
	if _, err := ProbeRunnerTree(dir); !errors.Is(err, ErrNoRunnerTree) {
		t.Fatalf("empty dir: %v", err)
	}
	writeRunnerTree(t, dir, "2.336.0")
	info, err := ProbeRunnerTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "2.336.0" || len(info.Parts) != 3 || info.Parts[1] != 336 || info.Dir != dir {
		t.Fatalf("%+v", info)
	}
}
