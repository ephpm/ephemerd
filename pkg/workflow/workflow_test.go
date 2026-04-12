package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

// --- Parse tests ---

func TestParse_ValidWorkflow(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "ci.yml")
	if err := os.WriteFile(path, []byte(`
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo hello
  test:
    runs-on: [self-hosted, linux, x64]
    env:
      EPHEMERD_IMAGE: ghcr.io/myorg/runner:latest
    steps:
      - run: make test
`), 0o644); err != nil {
		t.Fatal(err)
	}

	wf, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if wf.Name != "CI" {
		t.Errorf("Name = %q, want %q", wf.Name, "CI")
	}
	if len(wf.Jobs) != 2 {
		t.Errorf("Jobs count = %d, want 2", len(wf.Jobs))
	}

	build, ok := wf.Jobs["build"]
	if !ok {
		t.Fatal("missing 'build' job")
	}
	if build.RunsOn != "ubuntu-latest" {
		t.Errorf("build.RunsOn = %v, want %q", build.RunsOn, "ubuntu-latest")
	}
	if len(build.Steps) != 2 {
		t.Errorf("build.Steps count = %d, want 2", len(build.Steps))
	}

	test, ok := wf.Jobs["test"]
	if !ok {
		t.Fatal("missing 'test' job")
	}
	if test.Env["EPHEMERD_IMAGE"] != "ghcr.io/myorg/runner:latest" {
		t.Errorf("test.Env[EPHEMERD_IMAGE] = %q, want %q", test.Env["EPHEMERD_IMAGE"], "ghcr.io/myorg/runner:latest")
	}
}

func TestParse_NoJobs(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "empty.yml")
	if err := os.WriteFile(path, []byte(`
name: Empty
on: push
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Parse(path)
	if err == nil {
		t.Fatal("expected error for workflow with no jobs")
	}
}

func TestParse_InvalidYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "bad.yml")
	if err := os.WriteFile(path, []byte(`not: valid: yaml: {{`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Parse(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestParse_NonexistentFile(t *testing.T) {
	_, err := Parse("/nonexistent/workflow.yml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestParse_JobWithName(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "named.yml")
	if err := os.WriteFile(path, []byte(`
jobs:
  my-job:
    name: Build and Test
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`), 0o644); err != nil {
		t.Fatal(err)
	}

	wf, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	job := wf.Jobs["my-job"]
	if job.Name != "Build and Test" {
		t.Errorf("Name = %q, want %q", job.Name, "Build and Test")
	}
}

func TestParse_StepFields(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "steps.yml")
	if err := os.WriteFile(path, []byte(`
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v4
        with:
          fetch-depth: "0"
      - name: Run tests
        run: go test ./...
        env:
          CGO_ENABLED: "0"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	wf, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	steps := wf.Jobs["test"].Steps
	if len(steps) != 2 {
		t.Fatalf("steps count = %d, want 2", len(steps))
	}

	if steps[0].Name != "Checkout" {
		t.Errorf("step[0].Name = %q, want %q", steps[0].Name, "Checkout")
	}
	if steps[0].Uses != "actions/checkout@v4" {
		t.Errorf("step[0].Uses = %q", steps[0].Uses)
	}
	if steps[0].With["fetch-depth"] != "0" {
		t.Errorf("step[0].With[fetch-depth] = %q", steps[0].With["fetch-depth"])
	}

	if steps[1].Run != "go test ./..." {
		t.Errorf("step[1].Run = %q", steps[1].Run)
	}
	if steps[1].Env["CGO_ENABLED"] != "0" {
		t.Errorf("step[1].Env[CGO_ENABLED] = %q", steps[1].Env["CGO_ENABLED"])
	}
}

// --- FindWorkflow tests ---

func TestFindWorkflow_YML(t *testing.T) {
	tmp := t.TempDir()
	wfDir := filepath.Join(tmp, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte("name: CI\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := FindWorkflow(tmp)
	if err != nil {
		t.Fatalf("FindWorkflow() error: %v", err)
	}
	if filepath.Base(path) != "ci.yml" {
		t.Errorf("FindWorkflow() = %q, expected ci.yml", path)
	}
}

func TestFindWorkflow_YAML(t *testing.T) {
	tmp := t.TempDir()
	wfDir := filepath.Join(tmp, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "deploy.yaml"), []byte("name: Deploy"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := FindWorkflow(tmp)
	if err != nil {
		t.Fatalf("FindWorkflow() error: %v", err)
	}
	if filepath.Base(path) != "deploy.yaml" {
		t.Errorf("FindWorkflow() = %q, expected deploy.yaml", path)
	}
}

func TestFindWorkflow_NoWorkflows(t *testing.T) {
	tmp := t.TempDir()
	_, err := FindWorkflow(tmp)
	if err == nil {
		t.Fatal("expected error when no workflows found")
	}
}

func TestFindWorkflow_EmptyWorkflowDir(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := FindWorkflow(tmp)
	if err == nil {
		t.Fatal("expected error when workflow dir exists but is empty")
	}
}

func TestFindWorkflow_PrefersYML(t *testing.T) {
	tmp := t.TempDir()
	wfDir := filepath.Join(tmp, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "a.yml"), []byte("yml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "b.yaml"), []byte("yaml"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := FindWorkflow(tmp)
	if err != nil {
		t.Fatalf("FindWorkflow() error: %v", err)
	}
	// .yml patterns are checked first
	if filepath.Ext(path) != ".yml" {
		t.Errorf("FindWorkflow() = %q, expected .yml to be preferred", path)
	}
}

// --- TargetPlatform.String() tests ---

func TestTargetPlatform_String(t *testing.T) {
	tests := []struct {
		p    TargetPlatform
		want string
	}{
		{PlatformLinux, "linux"},
		{PlatformWindows, "windows"},
		{PlatformMacOS, "macos"},
		{TargetPlatform(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.p.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- normalizeRunsOn tests ---

func TestNormalizeRunsOn(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  int
	}{
		{"string", "ubuntu-latest", 1},
		{"[]string", []string{"self-hosted", "linux"}, 2},
		{"[]interface{}", []interface{}{"self-hosted", "linux", "x64"}, 3},
		{"nil", nil, 0},
		{"int", 42, 0},
		{"[]interface with non-string", []interface{}{"linux", 42}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeRunsOn(tt.input)
			if len(result) != tt.want {
				t.Errorf("normalizeRunsOn(%v) len = %d, want %d (got %v)", tt.input, len(result), tt.want, result)
			}
		})
	}
}
