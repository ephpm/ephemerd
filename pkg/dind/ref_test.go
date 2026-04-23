package dind

import "testing"

func TestNormalizeImageRef(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"docker-hub org repo with tag", "moby/buildkit:buildx-stable-1", "docker.io/moby/buildkit:buildx-stable-1"},
		{"docker-hub library with tag", "alpine:3.21", "docker.io/library/alpine:3.21"},
		{"docker-hub org repo no tag", "moby/buildkit", "docker.io/moby/buildkit:latest"},
		{"docker-hub library no tag", "alpine", "docker.io/library/alpine:latest"},
		{"already qualified ghcr with tag", "ghcr.io/actions/actions-runner:latest", "ghcr.io/actions/actions-runner:latest"},
		{"already qualified mcr with tag", "mcr.microsoft.com/windows/servercore:ltsc2025", "mcr.microsoft.com/windows/servercore:ltsc2025"},
		{"qualified registry port", "registry:5000/myimage:v1", "registry:5000/myimage:v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeImageRef(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("normalizeImageRef(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeImageRef_Invalid(t *testing.T) {
	cases := []string{
		"",
		":tag-only",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := normalizeImageRef(in); err == nil {
				t.Errorf("normalizeImageRef(%q) = nil error, want error", in)
			}
		})
	}
}
