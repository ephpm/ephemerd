//go:build linux

package networking

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- writeConfig tests ---

func TestWriteConfig_ProducesValidJSON(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "10-ephemerd.conflist")

	l := &linuxNetworking{
		cfg: Config{
			Subnet: "10.88.0.0/16",
			MTU:    1500,
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	if err := l.writeConfig(confPath); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("config is not valid JSON: %v\n%s", err, data)
	}

	if parsed["cniVersion"] != "1.0.0" {
		t.Errorf("cniVersion = %v, want 1.0.0", parsed["cniVersion"])
	}
	if parsed["name"] != "ephemerd" {
		t.Errorf("name = %v, want ephemerd", parsed["name"])
	}

	plugins, ok := parsed["plugins"].([]any)
	if !ok {
		t.Fatalf("plugins is not an array: %T", parsed["plugins"])
	}
	if len(plugins) != 2 {
		t.Errorf("plugin count = %d, want 2 (bridge + portmap)", len(plugins))
	}

	bridge, ok := plugins[0].(map[string]any)
	if !ok {
		t.Fatalf("first plugin is not an object")
	}
	if bridge["type"] != "bridge" {
		t.Errorf("bridge type = %v, want bridge", bridge["type"])
	}
	if bridge["bridge"] != defaultBridgeName {
		t.Errorf("bridge name = %v, want %s", bridge["bridge"], defaultBridgeName)
	}
	if bridge["isDefaultGateway"] != true {
		t.Errorf("isDefaultGateway = %v, want true", bridge["isDefaultGateway"])
	}
	if bridge["ipMasq"] != true {
		t.Errorf("ipMasq = %v, want true", bridge["ipMasq"])
	}

	pm, ok := plugins[1].(map[string]any)
	if !ok {
		t.Fatalf("second plugin is not an object")
	}
	if pm["type"] != "portmap" {
		t.Errorf("portmap type = %v, want portmap", pm["type"])
	}
}

func TestWriteConfig_GatewayMatchesSubnet(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "conflist.json")

	l := &linuxNetworking{
		cfg: Config{
			Subnet: "10.99.0.0/16",
			MTU:    1500,
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	if err := l.writeConfig(confPath); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	data, _ := os.ReadFile(confPath)
	if !strings.Contains(string(data), "10.99.0.1") {
		t.Errorf("config missing derived gateway 10.99.0.1: %s", data)
	}
	if !strings.Contains(string(data), "10.99.0.0/16") {
		t.Errorf("config missing subnet 10.99.0.0/16: %s", data)
	}
}

func TestWriteConfig_DefaultMTU(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "conf.json")

	// MTU not set in Config — l.mtu() should fall back to detectMTU().
	l := &linuxNetworking{
		cfg: Config{
			Subnet: "10.88.0.0/16",
			MTU:    0,
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	if err := l.writeConfig(confPath); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	data, _ := os.ReadFile(confPath)
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	plugins := parsed["plugins"].([]any)
	bridge := plugins[0].(map[string]any)

	mtu, ok := bridge["mtu"].(float64)
	if !ok {
		t.Fatalf("bridge.mtu is not a number: %T", bridge["mtu"])
	}
	if mtu <= 0 {
		t.Errorf("default MTU = %v, expected positive", mtu)
	}
}

// --- cleanStaleBridge tests ---

func TestCleanStaleBridge_NoPanic(t *testing.T) {
	// On a machine without an ephemerd0 bridge, cleanStaleBridge should
	// log a debug message and return without panicking. We can't make
	// strong assertions about a system-level command here — we just
	// verify it doesn't panic.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cleanStaleBridge(log)
}

// --- linuxNetworking.mtu tests ---

func TestLinuxNetworking_MTU_ExplicitOverride(t *testing.T) {
	l := &linuxNetworking{
		cfg: Config{MTU: 1280},
	}
	if got := l.mtu(); got != 1280 {
		t.Errorf("mtu() with explicit 1280 = %d, want 1280", got)
	}
}

func TestLinuxNetworking_MTU_DetectFallback(t *testing.T) {
	l := &linuxNetworking{cfg: Config{MTU: 0}}
	got := l.mtu()
	if got <= 0 {
		t.Errorf("mtu() fallback = %d, want positive", got)
	}
}
