package forgerunner

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Workflow is a parsed Actions workflow YAML.
type Workflow struct {
	Name string          `yaml:"name"`
	Jobs map[string]*Job `yaml:"jobs"`
}

// Job is a single job within a workflow.
type Job struct {
	Name   string            `yaml:"name"`
	RunsOn RunsOn            `yaml:"runs-on"`
	Steps  []*Step           `yaml:"steps"`
	Env    map[string]string `yaml:"env"`
	If     string            `yaml:"if"`
}

// Step is a single step within a job.
type Step struct {
	ID               string            `yaml:"id"`
	Name             string            `yaml:"name"`
	Run              string            `yaml:"run"`
	Shell            string            `yaml:"shell"`
	Env              map[string]string `yaml:"env"`
	WorkingDirectory string            `yaml:"working-directory"`
	If               string            `yaml:"if"`
	Uses             string            `yaml:"uses"`
	With             map[string]string `yaml:"with"`
}

// DisplayName returns the step's name, ID, or a fallback based on content.
func (s *Step) DisplayName(index int) string {
	if s.Name != "" {
		return s.Name
	}
	if s.ID != "" {
		return s.ID
	}
	if s.Uses != "" {
		return s.Uses
	}
	if s.Run != "" {
		r := s.Run
		if len(r) > 40 {
			r = r[:40] + "..."
		}
		return fmt.Sprintf("Run %s", r)
	}
	return fmt.Sprintf("Step %d", index+1)
}

// RunsOn handles the YAML runs-on field which can be a string or []string.
type RunsOn struct {
	Labels []string
}

func (r *RunsOn) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		r.Labels = []string{value.Value}
		return nil
	case yaml.SequenceNode:
		var labels []string
		if err := value.Decode(&labels); err != nil {
			return err
		}
		r.Labels = labels
		return nil
	default:
		return fmt.Errorf("runs-on: expected string or array, got %v", value.Kind)
	}
}

// ParseWorkflow parses workflow YAML bytes into a Workflow.
func ParseWorkflow(data []byte) (*Workflow, error) {
	var wf Workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	if len(wf.Jobs) == 0 {
		return nil, fmt.Errorf("parse workflow: no jobs defined")
	}
	return &wf, nil
}
