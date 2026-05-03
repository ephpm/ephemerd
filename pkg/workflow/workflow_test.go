package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
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
    container:
      image: ghcr.io/myorg/runner:latest
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
	if test.Container.Image != "ghcr.io/myorg/runner:latest" {
		t.Errorf("test.Container.Image = %q, want %q", test.Container.Image, "ghcr.io/myorg/runner:latest")
	}
}

func TestParse_ContainerAsString(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "shorthand.yml")
	if err := os.WriteFile(path, []byte(`
jobs:
  build:
    runs-on: ubuntu-latest
    container: ghcr.io/myorg/runner:v2
    steps:
      - run: echo hi
`), 0o644); err != nil {
		t.Fatal(err)
	}

	wf, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if wf.Jobs["build"].Container.Image != "ghcr.io/myorg/runner:v2" {
		t.Errorf("Container.Image = %q, want %q",
			wf.Jobs["build"].Container.Image, "ghcr.io/myorg/runner:v2")
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

// --- Container.UnmarshalYAML tests ---

func TestContainer_UnmarshalYAML_StringForm(t *testing.T) {
	// Bare string ("image:tag") shorthand.
	data := []byte(`ghcr.io/myorg/runner:v1`)
	var c Container
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if c.Image != "ghcr.io/myorg/runner:v1" {
		t.Errorf("Image = %q, want %q", c.Image, "ghcr.io/myorg/runner:v1")
	}
}

func TestContainer_UnmarshalYAML_StringForm_QuotedTag(t *testing.T) {
	// Quoted string preserves colon-bearing image refs.
	data := []byte(`"my/image:1.2.3"`)
	var c Container
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if c.Image != "my/image:1.2.3" {
		t.Errorf("Image = %q, want %q", c.Image, "my/image:1.2.3")
	}
}

func TestContainer_UnmarshalYAML_MappingForm(t *testing.T) {
	// Full mapping form with image key.
	data := []byte("image: ghcr.io/myorg/runner:latest\n")
	var c Container
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if c.Image != "ghcr.io/myorg/runner:latest" {
		t.Errorf("Image = %q, want %q", c.Image, "ghcr.io/myorg/runner:latest")
	}
}

func TestContainer_UnmarshalYAML_MappingForm_WithExtraFields(t *testing.T) {
	// Mapping with unknown fields — should still parse the image.
	// yaml.v3 ignores unknown fields by default.
	data := []byte(`
image: my-image:tag
options: --cpus 1
ports:
  - 8080:80
`)
	var c Container
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if c.Image != "my-image:tag" {
		t.Errorf("Image = %q, want %q", c.Image, "my-image:tag")
	}
}

func TestContainer_UnmarshalYAML_MappingForm_NoImage(t *testing.T) {
	// Mapping form with no image key — Image should be zero value.
	data := []byte("options: --rm\n")
	var c Container
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if c.Image != "" {
		t.Errorf("Image = %q, want empty", c.Image)
	}
}

func TestContainer_UnmarshalYAML_Null(t *testing.T) {
	// Explicit null — yaml.v3 represents this as a ScalarNode with empty value.
	data := []byte("~\n")
	var c Container
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	// Null ScalarNode produces empty string image (current behavior).
	if c.Image != "" {
		t.Errorf("Image = %q, want empty for null", c.Image)
	}
}

func TestContainer_UnmarshalYAML_EmptyString(t *testing.T) {
	// Empty quoted string.
	data := []byte(`""`)
	var c Container
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if c.Image != "" {
		t.Errorf("Image = %q, want empty", c.Image)
	}
}

func TestContainer_UnmarshalYAML_InvalidType_Sequence(t *testing.T) {
	// A sequence (list) is neither a scalar nor mapping — should error.
	data := []byte("[a, b, c]\n")
	var c Container
	err := yaml.Unmarshal(data, &c)
	if err == nil {
		t.Fatal("expected error for sequence-typed container")
	}
}

func TestContainer_UnmarshalYAML_NestedJob(t *testing.T) {
	// Container nested inside a Job (real workflow shape).
	data := []byte(`
runs-on: ubuntu-latest
container:
  image: nested/image:v1
steps:
  - run: echo hi
`)
	var j Job
	if err := yaml.Unmarshal(data, &j); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if j.Container.Image != "nested/image:v1" {
		t.Errorf("nested Container.Image = %q, want %q", j.Container.Image, "nested/image:v1")
	}
}

func TestContainer_UnmarshalYAML_NestedJobShorthand(t *testing.T) {
	// Container as shorthand string nested in a Job.
	data := []byte(`
runs-on: ubuntu-latest
container: shorthand/image:tag
`)
	var j Job
	if err := yaml.Unmarshal(data, &j); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if j.Container.Image != "shorthand/image:tag" {
		t.Errorf("Container.Image = %q, want %q", j.Container.Image, "shorthand/image:tag")
	}
}

func TestContainer_UnmarshalYAML_DoesNotInfiniteLoop(t *testing.T) {
	// Regression: the implementation uses a `type raw Container` alias to
	// avoid recursing into its own UnmarshalYAML. Without it, this test
	// would stack-overflow.
	data := []byte("image: a\n")
	for i := 0; i < 1000; i++ {
		var c Container
		if err := yaml.Unmarshal(data, &c); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
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
