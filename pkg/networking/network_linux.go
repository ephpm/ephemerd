//go:build linux

package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"

	cni "github.com/containerd/go-cni"
)

const defaultBridgeName = "ephemerd0"

type linuxNetworking struct {
	cfg Config
	cni cni.CNI
}

func newPlatformNetworking() platformNetworking {
	return &linuxNetworking{}
}

func (l *linuxNetworking) init(cfg Config) error {
	l.cfg = cfg

	confDir := filepath.Join(cfg.DataDir, "cni", "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return fmt.Errorf("creating CNI conf dir: %w", err)
	}

	confPath := filepath.Join(confDir, "10-ephemerd.conflist")
	if err := l.writeConfig(confPath); err != nil {
		return fmt.Errorf("writing CNI config: %w", err)
	}

	data, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("reading CNI config: %w", err)
	}

	opts := []cni.Opt{}
	if cfg.CNIBinDir != "" {
		opts = append(opts, cni.WithPluginDir([]string{cfg.CNIBinDir}))
	}
	opts = append(opts,
		cni.WithConfListBytes(data),
		cni.WithLoNetwork,
	)

	network, err := cni.New(opts...)
	if err != nil {
		return fmt.Errorf("initializing CNI: %w", err)
	}
	l.cni = network

	cfg.Log.Info("CNI networking initialized", "bridge", defaultBridgeName, "subnet", cfg.subnet())
	return nil
}

func (l *linuxNetworking) setup(ctx context.Context, id string, netns string) (*SetupResult, error) {
	result, err := l.cni.Setup(ctx, id, netns)
	if err != nil {
		return nil, fmt.Errorf("CNI setup for %s: %w", id, err)
	}

	l.cfg.Log.Debug("network attached", "id", id, "ips", result.Interfaces)
	return &SetupResult{NetNS: netns}, nil
}

func (l *linuxNetworking) teardown(ctx context.Context, id string, netns string) error {
	if err := l.cni.Remove(ctx, id, netns); err != nil {
		return fmt.Errorf("CNI teardown for %s: %w", id, err)
	}
	return nil
}

func (l *linuxNetworking) writeConfig(path string) error {
	subnet := l.cfg.subnet()
	gateway := deriveGateway(subnet)

	conflist := map[string]any{
		"cniVersion": "1.0.0",
		"name":       "ephemerd",
		"plugins": []map[string]any{
			{
				"type":             "bridge",
				"bridge":           defaultBridgeName,
				"isDefaultGateway": true,
				"ipMasq":           true,
				"hairpinMode":      true,
				"mtu":              detectMTU(),
				"ipam": map[string]any{
					"type":   "host-local",
					"ranges": [][]map[string]string{{{
						"subnet":  subnet,
						"gateway": gateway,
					}}},
				},
			},
			{
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

// detectMTU finds the MTU of the default network interface.
// Container bridges must match the host's MTU or large packets
// (like TLS handshakes) get silently dropped.
func detectMTU() int {
	ifaces, err := net.Interfaces()
	if err != nil {
		return 1500
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Name == defaultBridgeName {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil || len(addrs) == 0 {
			continue
		}
		if iface.MTU > 0 && iface.MTU < 1500 {
			return iface.MTU
		}
	}
	return 1500
}

// deriveGateway returns the first usable IP in the subnet (x.x.x.1).
func deriveGateway(subnet string) string {
	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		return "10.89.0.1"
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return "10.89.0.1"
	}
	ip4[3] = 1
	return ip4.String()
}
