//go:build !windows

package main

import "io"

// runAsWindowsService is a no-op on non-Windows platforms.
func runAsWindowsService() bool { return false }

// getServiceLogWriter returns nil on non-Windows (no Event Log).
func getServiceLogWriter() io.Writer { return nil }
