package containerd

import (
	goruntime "runtime"
	"testing"
)

func TestCrictlEndpoint_Linux(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Linux socket form")
	}
	got := crictlEndpoint("/var/lib/ephemerd/containerd/containerd.sock")
	want := "unix:///var/lib/ephemerd/containerd/containerd.sock"
	if got != want {
		t.Errorf("crictlEndpoint = %q, want %q", got, want)
	}
}

func TestCrictlEndpoint_Windows(t *testing.T) {
	if goruntime.GOOS != "windows" {
		t.Skip("Windows named-pipe form")
	}
	got := crictlEndpoint(`\\.\pipe\ephemerd-containerd`)
	want := "npipe:////./pipe/ephemerd-containerd"
	if got != want {
		t.Errorf("crictlEndpoint = %q, want %q", got, want)
	}
}
