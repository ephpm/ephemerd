//go:build linux

package main

import (
	"log/slog"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/scheduler"
)

// startContainerRuntime starts an in-process containerd server on Linux.
func startContainerRuntime(dataDir string, log *slog.Logger, _ bool, tcpPort uint32, _ bool) (*client.Client, func() *scheduler.DispatchClient, func(), error) {
	ctrd, err := containerd.New(containerd.Config{
		DataDir: dataDir,
		TCPPort: tcpPort,
		Log:     log,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	cleanup := func() { ctrd.Stop() }
	return ctrd.Client(), func() *scheduler.DispatchClient { return nil }, cleanup, nil
}
