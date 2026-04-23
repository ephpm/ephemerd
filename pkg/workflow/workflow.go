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
	Name      string            `yaml:"name"`
	RunsOn    interface{}       `yaml:"runs-on"` // string or []string
	Env       map[string]string `yaml:"env"`
	Container Container         `yaml:"container"`
	Steps     []Step            `yaml:"steps"`
}

// Container specifies the OCI image to use for the job.
//
// On Linux and Windows hosts, this is the runner container image. On macOS
// hosts, it is an OCI artifact image whose layers are extracted onto the
// per-job VM via virtio-fs.
//
// GitHub Actions accepts both a bare string (shorthand for {image: "..."})
// and a full object. The custom UnmarshalYAML below handles both forms.
type Container struct {
	Image string `yaml:"image"`
}

// UnmarshalYAML accepts either a bare string ("image:tag") or a full mapping
// ({image: "...", ...}) for the container field, matching GitHub Actions.
func (c *Container) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		c.Image = node.Value
		return nil
	case yaml.MappingNode:
		// Use an alias type to avoid recursing into our own UnmarshalYAML.
		type raw Container
		var r raw
		if err := node.Decode(&r); err != nil {
			return err
		}
		*c = Container(r)
		return nil
	default:
		return fmt.Errorf("container: expected string or mapping, got %v", node.Kind)
	}
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
