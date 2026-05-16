package github

import "testing"

func TestHasEvent(t *testing.T) {
	tests := []struct {
		name   string
		events []string
		target string
		want   bool
	}{
		{
			name:   "found",
			events: []string{"push", "workflow_job", "issues"},
			target: "workflow_job",
			want:   true,
		},
		{
			name:   "not found",
			events: []string{"push", "issues"},
			target: "workflow_job",
			want:   false,
		},
		{
			name:   "empty list",
			events: []string{},
			target: "workflow_job",
			want:   false,
		},
		{
			name:   "nil list",
			events: nil,
			target: "workflow_job",
			want:   false,
		},
		{
			name:   "first element",
			events: []string{"workflow_job"},
			target: "workflow_job",
			want:   true,
		},
		{
			name:   "case-sensitive — no match",
			events: []string{"Workflow_Job"},
			target: "workflow_job",
			want:   false,
		},
		{
			name:   "empty target with empty entry matches",
			events: []string{""},
			target: "",
			want:   true,
		},
		{
			name:   "empty target without empty entry",
			events: []string{"push"},
			target: "",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasEvent(tt.events, tt.target); got != tt.want {
				t.Errorf("hasEvent(%v, %q) = %v, want %v", tt.events, tt.target, got, tt.want)
			}
		})
	}
}
