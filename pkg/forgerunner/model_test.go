package forgerunner

import "testing"

func TestParseWorkflow(t *testing.T) {
	yaml := []byte(`name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    env:
      FOO: bar
    steps:
      - name: Hello
        run: echo hello
      - id: step2
        run: echo step2
        shell: bash
        env:
          BAZ: qux
  test:
    runs-on:
      - self-hosted
      - linux
    steps:
      - run: go test ./...
`)

	wf, err := ParseWorkflow(yaml)
	if err != nil {
		t.Fatalf("ParseWorkflow: %v", err)
	}
	if wf.Name != "CI" {
		t.Errorf("Name = %q, want CI", wf.Name)
	}
	if len(wf.Jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(wf.Jobs))
	}

	build := wf.Jobs["build"]
	if build == nil {
		t.Fatal("missing build job")
	}
	if len(build.RunsOn.Labels) != 1 || build.RunsOn.Labels[0] != "ubuntu-latest" {
		t.Errorf("build.RunsOn = %v, want [ubuntu-latest]", build.RunsOn.Labels)
	}
	if build.Env["FOO"] != "bar" {
		t.Errorf("build.Env[FOO] = %q, want bar", build.Env["FOO"])
	}
	if len(build.Steps) != 2 {
		t.Fatalf("build has %d steps, want 2", len(build.Steps))
	}
	if build.Steps[0].Name != "Hello" {
		t.Errorf("step[0].Name = %q, want Hello", build.Steps[0].Name)
	}
	if build.Steps[1].Shell != "bash" {
		t.Errorf("step[1].Shell = %q, want bash", build.Steps[1].Shell)
	}

	test := wf.Jobs["test"]
	if test == nil {
		t.Fatal("missing test job")
	}
	if len(test.RunsOn.Labels) != 2 {
		t.Errorf("test.RunsOn = %v, want [self-hosted, linux]", test.RunsOn.Labels)
	}
}

func TestParseWorkflow_NoJobs(t *testing.T) {
	_, err := ParseWorkflow([]byte("name: empty\n"))
	if err == nil {
		t.Fatal("expected error for no jobs")
	}
}

func TestParseWorkflow_Invalid(t *testing.T) {
	_, err := ParseWorkflow([]byte("{{invalid yaml"))
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestStep_DisplayName(t *testing.T) {
	tests := []struct {
		step *Step
		want string
	}{
		{&Step{Name: "Build"}, "Build"},
		{&Step{ID: "build-step"}, "build-step"},
		{&Step{Uses: "actions/checkout@v4"}, "actions/checkout@v4"},
		{&Step{Run: "echo hello"}, "Run echo hello"},
		{&Step{Run: "this is a very long script that exceeds forty characters in length"}, "Run this is a very long script that exceeds ..."},
		{&Step{}, "Step 1"},
	}
	for _, tt := range tests {
		got := tt.step.DisplayName(0)
		if got != tt.want {
			t.Errorf("DisplayName() = %q, want %q", got, tt.want)
		}
	}
}
