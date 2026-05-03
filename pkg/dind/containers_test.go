package dind

import (
	"context"
	"reflect"
	"regexp"
	"testing"

	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

func TestWithBindMount_AppendsToNilMounts(t *testing.T) {
	spec := &ocispec.Spec{}
	opt := withBindMount("/host/src", "/container/dst", []string{"rbind", "ro"})

	if err := opt(context.Background(), nil, nil, spec); err != nil {
		t.Fatalf("opt: %v", err)
	}

	if len(spec.Mounts) != 1 {
		t.Fatalf("len(Mounts) = %d, want 1", len(spec.Mounts))
	}
	got := spec.Mounts[0]
	want := ocispec.Mount{
		Destination: "/container/dst",
		Type:        "bind",
		Source:      "/host/src",
		Options:     []string{"rbind", "ro"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mount = %#v, want %#v", got, want)
	}
}

func TestWithBindMount_AppendsToExistingMounts(t *testing.T) {
	spec := &ocispec.Spec{
		Mounts: []ocispec.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
		},
	}
	opt := withBindMount("/a", "/b", []string{"rbind"})

	if err := opt(context.Background(), nil, nil, spec); err != nil {
		t.Fatalf("opt: %v", err)
	}

	if len(spec.Mounts) != 2 {
		t.Fatalf("len(Mounts) = %d, want 2", len(spec.Mounts))
	}
	if spec.Mounts[0].Destination != "/proc" {
		t.Errorf("existing mount clobbered: %#v", spec.Mounts[0])
	}
	if spec.Mounts[1].Destination != "/b" || spec.Mounts[1].Source != "/a" {
		t.Errorf("appended mount = %#v", spec.Mounts[1])
	}
}

func TestWithBindMount_NilOptions(t *testing.T) {
	spec := &ocispec.Spec{}
	opt := withBindMount("/a", "/b", nil)

	if err := opt(context.Background(), nil, nil, spec); err != nil {
		t.Fatalf("opt: %v", err)
	}
	if spec.Mounts[0].Options != nil {
		t.Errorf("Options = %#v, want nil", spec.Mounts[0].Options)
	}
}

func TestGenerateContainerID_Shape(t *testing.T) {
	id := generateContainerID()
	// Default path: 32 random bytes hex-encoded → 64 hex chars.
	if matched, _ := regexp.MatchString(`^[0-9a-f]{64}$`, id); !matched {
		t.Errorf("id = %q, want 64-char hex string", id)
	}
}

func TestGenerateContainerID_Unique(t *testing.T) {
	const n = 64
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := generateContainerID()
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}
