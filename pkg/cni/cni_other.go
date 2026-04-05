//go:build !linux

package cni

import "log/slog"

// Version is set at build time via ldflags.
var Version = "unknown"

// Manager is a no-op on non-Linux platforms.
// Windows uses HCN and macOS uses VM NAT — neither needs CNI plugins.
type Manager struct{}

// New creates a no-op CNI plugin manager.
func New(_ string, _ *slog.Logger) *Manager { return &Manager{} }

// Dir returns empty string on non-Linux platforms.
func (m *Manager) Dir() string { return "" }

// Extract is a no-op on non-Linux platforms.
func (m *Manager) Extract() error { return nil }
