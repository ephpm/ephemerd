//go:build linux

package buildkit

import (
	"path/filepath"
	"testing"
)

// TestBuildkitNetProviderOpt_HostNetnsWhenNoCNI documents the pre-fix default:
// with no CNI config path, the worker uses "auto" mode, which
// netproviders.Providers resolves to the HOST network namespace when no config
// file is present — the egress-firewall bypass this fix targets. The test pins
// the decision so a regression back to unconditional "auto" is caught.
func TestBuildkitNetProviderOpt_HostNetnsWhenNoCNI(t *testing.T) {
	opt := buildkitNetProviderOpt(Config{DataDir: "/tmp/bk"})
	if opt.Mode != "auto" {
		t.Errorf("Mode = %q, want auto when no CNI config path", opt.Mode)
	}
}

// TestBuildkitNetProviderOpt_CNIWhenConfigured asserts that supplying a CNI
// config path forces mode "cni" (never "auto", which could silently fall back
// to host netns) and threads the config/binary/root paths through.
func TestBuildkitNetProviderOpt_CNIWhenConfigured(t *testing.T) {
	cfg := Config{
		DataDir:       "/var/lib/ephemerd/buildkit",
		CNIConfigPath: "/var/lib/ephemerd/cni/conf/10-ephemerd.conflist",
		CNIBinDir:     "/opt/cni/bin",
	}
	opt := buildkitNetProviderOpt(cfg)

	if opt.Mode != "cni" {
		t.Fatalf("Mode = %q, want cni", opt.Mode)
	}
	if opt.Mode == "host" {
		t.Fatal("Mode must never be host when a CNI config is configured")
	}
	if opt.CNI.ConfigPath != cfg.CNIConfigPath {
		t.Errorf("CNI.ConfigPath = %q, want %q", opt.CNI.ConfigPath, cfg.CNIConfigPath)
	}
	if opt.CNI.BinaryDir != cfg.CNIBinDir {
		t.Errorf("CNI.BinaryDir = %q, want %q", opt.CNI.BinaryDir, cfg.CNIBinDir)
	}
	if want := filepath.Join(cfg.DataDir, "cni"); opt.CNI.Root != want {
		t.Errorf("CNI.Root = %q, want %q", opt.CNI.Root, want)
	}
}

// TestBuildkitDNSOpt_EmptyIsNilNonEmptySet pins the pairing that goes with
// CNI mode: once builds move off the host netns (above), they can only resolve
// names if the executor writes an explicit resolv.conf. Empty must stay nil
// (BuildKit's host-resolv.conf default, correct only for host-netns builds);
// a configured gateway must produce a DNSConfig carrying it — the regression
// where every `RUN apt-get` fails with "Temporary failure resolving".
func TestBuildkitDNSOpt_EmptyIsNilNonEmptySet(t *testing.T) {
	if got := buildkitDNSOpt(nil); got != nil {
		t.Errorf("buildkitDNSOpt(nil) = %+v, want nil (host resolv.conf default)", got)
	}
	if got := buildkitDNSOpt([]string{}); got != nil {
		t.Errorf("buildkitDNSOpt([]) = %+v, want nil", got)
	}
	got := buildkitDNSOpt([]string{"10.88.0.1"})
	if got == nil {
		t.Fatal("buildkitDNSOpt([gateway]) = nil — build containers would fall back to the unreachable host resolver")
	}
	if len(got.Nameservers) != 1 || got.Nameservers[0] != "10.88.0.1" {
		t.Errorf("Nameservers = %v, want [10.88.0.1]", got.Nameservers)
	}
}
