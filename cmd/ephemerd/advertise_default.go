//go:build linux

package main

import "log/slog"

// resolveJobProxyEnv returns the proxy env unchanged on Linux.
//
// Job containers here sit on this host's CNI bridge, so the advertised bridge
// gateway is already the right address. It deliberately is NOT probed: the
// bridge does not exist at daemon start — CNI creates it with the first job
// container (see pkg/proxies/listen.go) — so a reachability check would fail
// every time and disable a set of proxies that work perfectly.
func resolveJobProxyEnv(env []string, _ *slog.Logger) []string { return env }
