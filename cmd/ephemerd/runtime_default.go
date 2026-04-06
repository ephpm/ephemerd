//go:build linux

package main

import (
	"log/slog"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/containerd"
)

// startContainerRuntime starts an in-process containerd server on Linux.
func startContainerRuntime(dataDir string, log *slog.Logger, _ bool) (*client.Client, func(), error) {
	ctrd, err := containerd.New(containerd.Config{
		DataDir: dataDir,
		Log:     log,
	})
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() { ctrd.Stop() }
	return ctrd.Client(), cleanup, nil
}
