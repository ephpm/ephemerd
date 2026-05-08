//go:build linux

package main

import (
	"log/slog"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/scheduler"
)

// startContainerRuntime starts an in-process containerd server on Linux.
func startContainerRuntime(dataDir string, log *slog.Logger, _ bool, tcpPort uint32, tcpAddr string, _ bool, _ uint, _ uint64, _ uint64) (*client.Client, func() (*scheduler.DispatchClient, *client.Client), func(), error) {
	ctrd, err := containerd.New(containerd.Config{
		DataDir: dataDir,
		TCPPort: tcpPort,
		TCPAddr: tcpAddr,
		Log:     log,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	cleanup := func() { ctrd.Stop() }
	return ctrd.Client(), func() (*scheduler.DispatchClient, *client.Client) { return nil, nil }, cleanup, nil
}
