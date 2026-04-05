package workflow

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Workflow represents a parsed GitHub Actions workflow file.
type Workflow struct {
	Name string           `yaml:"name"`
	Jobs map[string]Job   `yaml:"jobs"`
}

// Job represents a single job within a workflow.
type Job struct {
	Name   string            `yaml:"name"`
	RunsOn interface{}       `yaml:"runs-on"` // string or []string
	Env    map[string]string `yaml:"env"`
	Steps  []Step            `yaml:"steps"`
}

// Step represents a single step within a job.
type Step struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

// Parse reads and parses a GitHub Actions workflow YAML file.
func Parse(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow file %s: %w", path, err)
	}

	var wf Workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parsing workflow file %s: %w", path, err)
	}

	if len(wf.Jobs) == 0 {
		return nil, fmt.Errorf("workflow %s has no jobs defined", path)
	}

	return &wf, nil
}

// FindWorkflow searches for a workflow file in the given directory.
// If dir contains .github/workflows/*.yml or *.yaml, the first match is returned.
func FindWorkflow(dir string) (string, error) {
	patterns := []string{
		filepath.Join(dir, ".github", "workflows", "*.yml"),
		filepath.Join(dir, ".github", "workflows", "*.yaml"),
	}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", fmt.Errorf("globbing %s: %w", pattern, err)
		}
		if len(matches) > 0 {
			return matches[0], nil
		}
	}

	return "", fmt.Errorf("no workflow files found in %s/.github/workflows/", dir)
}
