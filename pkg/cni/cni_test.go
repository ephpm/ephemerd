package cni

import (
	"log/slog"
	"runtime"
	"testing"
)

func TestNew(t *testing.T) {
	m := New("/data", slog.Default())
	if m == nil {
		t.Fatal("New() returned nil")
	}
}

func TestExtract_NoOp(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("skipping no-op test on Linux (real implementation)")
	}
	m := New("/data", slog.Default())
	if err := m.Extract(); err != nil {
		t.Errorf("Extract() on non-Linux should be no-op, got: %v", err)
	}
}

func TestDir(t *testing.T) {
	m := New("/data", slog.Default())
	dir := m.Dir()
	if runtime.GOOS == "linux" {
		if dir == "" {
			t.Error("Dir() on Linux should return a path")
		}
	} else {
		if dir != "" {
			t.Errorf("Dir() on non-Linux = %q, want empty", dir)
		}
	}
}

func TestVersion(t *testing.T) {
	// Version is set at build time; in tests it should be the default
	if Version == "" {
		t.Error("Version should not be empty")
	}
}
