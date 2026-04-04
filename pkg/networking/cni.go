package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	cni "github.com/containerd/go-cni"
)

const (
	// DefaultBridgeName is the Linux bridge created for ephemerd containers.
	DefaultBridgeName = "ephemerd0"

	// DefaultSubnet is the IP range for containers.
	DefaultSubnet = "10.88.0.0/16"

	// DefaultGateway is the bridge IP / default gateway for containers.
	DefaultGateway = "10.88.0.1"
)

// Config for container networking.
type Config struct {
	DataDir string
	Log     *slog.Logger
}

// Manager handles CNI-based container networking.
type Manager struct {
	cfg  Config
	cni  cni.CNI
	path string // path to generated CNI config
}

// New creates and initializes the CNI networking manager.
func New(cfg Config) (*Manager, error) {
	m := &Manager{
		cfg: cfg,
	}

	confDir := filepath.Join(cfg.DataDir, "cni", "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating CNI conf dir: %w", err)
	}

	// Write the CNI config
	confPath := filepath.Join(confDir, "10-ephemerd.conflist")
	if err := m.writeConfig(confPath); err != nil {
		return nil, fmt.Errorf("writing CNI config: %w", err)
	}
	m.path = confPath

	// Initialize CNI library
	network, err := cni.New(
		cni.WithConfListBytes(mustReadFile(confPath)),
		cni.WithLoNetwork,
	)
	if err != nil {
		return nil, fmt.Errorf("initializing CNI: %w", err)
	}
	m.cni = network

	cfg.Log.Info("CNI networking initialized", "bridge", DefaultBridgeName, "subnet", DefaultSubnet)
	return m, nil
}

// Setup attaches a container to the network. Call this after creating the
// container's network namespace but before starting the task.
// Returns the namespace path for use with the OCI spec.
func (m *Manager) Setup(ctx context.Context, id string, netns string) (*cni.Result, error) {
	result, err := m.cni.Setup(ctx, id, netns)
	if err != nil {
		return nil, fmt.Errorf("CNI setup for %s: %w", id, err)
	}

	m.cfg.Log.Debug("network attached", "id", id, "ips", result.Interfaces)
	return result, nil
}

// Teardown detaches a container from the network.
func (m *Manager) Teardown(ctx context.Context, id string, netns string) error {
	if err := m.cni.Remove(ctx, id, netns); err != nil {
		return fmt.Errorf("CNI teardown for %s: %w", id, err)
	}
	return nil
}

// writeConfig generates the CNI conflist with bridge, firewall, and portmap.
func (m *Manager) writeConfig(path string) error {
	conflist := map[string]any{
		"cniVersion": "1.0.0",
		"name":       "ephemerd",
		"plugins": []map[string]any{
			{
				// Bridge plugin: creates a veth pair and connects to a Linux bridge
				"type":             "bridge",
				"bridge":           DefaultBridgeName,
				"isDefaultGateway": true,
				"ipMasq":           true,
				"hairpinMode":      true,
				"ipam": map[string]any{
					"type":   "host-local",
					"ranges": [][]map[string]string{{{
						"subnet":  DefaultSubnet,
						"gateway": DefaultGateway,
					}}},
				},
			},
			{
				// Firewall plugin: applies iptables rules
				// Combined with iptables deny rules below
				"type": "firewall",
			},
			{
				// Portmap: allows port forwarding if needed
				"type":         "portmap",
				"capabilities": map[string]bool{"portMappings": true},
			},
		},
	}

	data, err := json.MarshalIndent(conflist, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling CNI config: %w", err)
	}

	return os.WriteFile(path, data, 0o644)
}

func mustReadFile(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("reading %s: %v", path, err))
	}
	return data
}
