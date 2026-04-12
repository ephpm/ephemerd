package scheduler

import (
	"runtime"
	"testing"
)

func TestIsMacOSJob(t *testing.T) {
	tests := []struct {
		labels []string
		want   bool
	}{
		{[]string{"self-hosted", "macos", "arm64"}, true},
		{[]string{"self-hosted", "macosx"}, true},
		{[]string{"macos-14"}, true},
		{[]string{"macos-latest"}, true},
		{[]string{"self-hosted", "MACOS", "ARM64"}, true},
		{[]string{"self-hosted", "linux", "x64"}, false},
		{[]string{"ubuntu-latest"}, false},
		{[]string{"windows-latest"}, false},
		{[]string{"self-hosted"}, false},
		{nil, false},
		{[]string{}, false},
	}

	for _, tt := range tests {
		got := isMacOSJob(tt.labels)
		if got != tt.want {
			t.Errorf("isMacOSJob(%v) = %v, want %v", tt.labels, got, tt.want)
		}
	}
}

func expectedArchLabel() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x64"
}

func TestBuildLabelsForOS_Darwin(t *testing.T) {
	labels := buildLabelsForOS("darwin", []string{"gpu"})

	if labels[0] != "self-hosted" {
		t.Errorf("labels[0] = %q, want self-hosted", labels[0])
	}
	if labels[1] != "macos" {
		t.Errorf("labels[1] = %q, want macos", labels[1])
	}
	if labels[2] != expectedArchLabel() {
		t.Errorf("labels[2] = %q, want %q", labels[2], expectedArchLabel())
	}
	if labels[3] != "gpu" {
		t.Errorf("labels[3] = %q, want gpu", labels[3])
	}
}

func TestBuildLabelsForOS_Linux(t *testing.T) {
	labels := buildLabelsForOS("linux", nil)
	if labels[1] != "linux" {
		t.Errorf("expected linux label, got %v", labels)
	}
}

func TestBuildLabelsForOS_Windows(t *testing.T) {
	labels := buildLabelsForOS("windows", nil)
	if labels[1] != "windows" {
		t.Errorf("expected windows label, got %v", labels)
	}
}
