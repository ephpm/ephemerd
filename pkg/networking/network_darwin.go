//go:build darwin

package networking

import (
	"context"
	"fmt"
)

type darwinNetworking struct {
	cfg Config
}

func newPlatformNetworking() platformNetworking {
	return &darwinNetworking{}
}

func (d *darwinNetworking) init(cfg Config) error {
	d.cfg = cfg
	cfg.Log.Warn("container networking not yet supported on macOS")
	return nil
}

func (d *darwinNetworking) setup(ctx context.Context, id string, netns string) (*SetupResult, error) {
	return nil, fmt.Errorf("container networking not supported on macOS")
}

func (d *darwinNetworking) teardown(ctx context.Context, id string, netns string) error {
	return nil
}

func (d *darwinNetworking) installFirewallRules() error {
	return nil
}

func (d *darwinNetworking) removeFirewallRules() {}
