//go:build linux

package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	cni "github.com/containerd/go-cni"
)

const defaultBridgeName = "ephemerd0"

type linuxNetworking struct {
	cfg Config
	cni cni.CNI

	// c2cMu guards the container-to-container membership maps and their
	// EPHEMERD-C2C rules. Both callers of joinJobNetwork/leaveJobNetwork run
	// concurrently (the runner attaches one container while a dind server
	// attaches its siblings), so mutation of the shared iptables chain has to
	// serialize.
	c2cMu sync.Mutex
	// c2cMember maps a CNI attachment id (the key Setup/Teardown use) to the
	// job it belongs to and the container IP it was given. Keyed by the SAME id
	// passed to Setup, so leaveJobNetwork needs only that id — the caller does
	// not have to remember the job or the address at teardown.
	c2cMember map[string]c2cMember
	// c2cJob maps a jobID to the set of container IPs currently attached for it.
	// Used to enumerate a joining container's intra-job peers.
	c2cJob map[string]map[string]bool
}

// c2cMember records which job a CNI-attached container belongs to and the IP it
// was assigned, so its intra-job allow rules can be removed on teardown.
type c2cMember struct {
	jobID string
	ip    string
}

func newPlatformNetworking() platformNetworking {
	return &linuxNetworking{}
}

func (l *linuxNetworking) init(cfg Config) error {
	l.cfg = cfg

	confPath := CNIConfListPath(cfg.DataDir)
	confDir := filepath.Dir(confPath)
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return fmt.Errorf("creating CNI conf dir: %w", err)
	}

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

	// Extract container IP from the CNI result.
	//
	// result.Interfaces is a map keyed by interface name and includes the
	// loopback interface ("lo") added by cni.WithLoNetwork. Map iteration order
	// is randomized in Go, so a naive "first IP wins" loop will sometimes
	// return 127.0.0.1 — that gets reported up to docker inspect and confuses
	// any caller (e.g. kind) that picks the node IP from there.
	//
	// Skip loopback addresses explicitly: keep only routable IPs from the
	// CNI bridge.
	var ip string
	for _, iface := range result.Interfaces {
		for _, ipCfg := range iface.IPConfigs {
			if ipCfg.IP == nil || ipCfg.IP.IsLoopback() {
				continue
			}
			ip = ipCfg.IP.String()
			break
		}
		if ip != "" {
			break
		}
	}

	l.cfg.Log.Debug("network attached", "id", id, "ip", ip)
	return &SetupResult{NetNS: netns, IP: ip}, nil
}

func (l *linuxNetworking) teardown(ctx context.Context, id string, netns string) error {
	if err := l.cni.Remove(ctx, id, netns); err != nil {
		return fmt.Errorf("CNI teardown for %s: %w", id, err)
	}
	return nil
}

// hostAddr: no L2Bridge on Linux — the generic subnet derivation applies.
func (l *linuxNetworking) hostAddr() string { return "" }

// openHostPort/closeHostPort live in firewall_linux.go alongside the rest of
// the iptables handling.

func (l *linuxNetworking) cleanup() {
	log := l.cfg.Log

	// Remove CNI config
	confDir := filepath.Join(l.cfg.DataDir, "cni", "conf")
	if err := os.RemoveAll(confDir); err != nil {
		log.Debug("failed to remove CNI config dir", "error", err)
	}

	// Remove CNI IP allocations (host-local state) but keep extracted plugins
	cniDir := filepath.Join(l.cfg.DataDir, "cni")
	entries, _ := os.ReadDir(cniDir)
	for _, e := range entries {
		if e.Name() == "bin" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(cniDir, e.Name())); err != nil {
			log.Debug("failed to remove CNI state", "name", e.Name(), "error", err)
		}
	}

	// Remove DNS config files
	dnsDir := filepath.Join(l.cfg.DataDir, "dns")
	if err := os.RemoveAll(dnsDir); err != nil {
		log.Debug("failed to remove DNS dir", "error", err)
	}

	// Delete bridge interface
	if err := exec.Command("ip", "link", "del", defaultBridgeName).Run(); err != nil {
		log.Debug("failed to delete bridge", "bridge", defaultBridgeName, "error", err)
	}

	log.Info("networking cleaned up")
}

func cleanStaleBridge(log *slog.Logger) {
	if err := exec.Command("ip", "link", "del", defaultBridgeName).Run(); err != nil {
		log.Debug("no stale bridge to clean", "bridge", defaultBridgeName)
	} else {
		log.Info("deleted stale bridge", "bridge", defaultBridgeName)
	}
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
				"mtu":              l.mtu(),
				"ipam": map[string]any{
					"type":   "host-local",
					"ranges": [][]map[string]string{{{
						"subnet":  subnet,
						"gateway": gateway,
					}}},
				},
				// Hand containers a resolv.conf pointing at the bridge gateway,
				// where ephemerd's own DNS listens (EPHEMERD-INPUT already
				// allows udp/tcp :53 to it). The bridge plugin copies this into
				// the CNI result, and BuildKit's CNI network provider writes it
				// into each build sandbox's /etc/resolv.conf.
				//
				// Without it, containers that rely on the CNI result for DNS —
				// specifically `docker build` RUN steps, which since #177 run on
				// this bridge instead of the host netns — get an empty resolver
				// and cannot resolve anything (e.g. github.com), breaking source
				// fetches mid-build. The runner container itself gets DNS by a
				// separate bind-mount (withDNSMount) and was unaffected; this
				// closes the gap for the build path.
				"dns": map[string]any{
					"nameservers": []string{gateway},
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

func (l *linuxNetworking) mtu() int {
	if l.cfg.MTU > 0 {
		return l.cfg.MTU
	}
	return detectMTU()
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
