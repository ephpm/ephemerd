//go:build windows

package tunnel

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// reapStrayCloudflared kills cloudflared processes left behind by a PREVIOUS
// ephemerd, before this one starts its own.
//
// The Job Object in bindChildLifetime protects children THIS binary spawns. It
// does nothing about children an older binary spawned, or about a bind that
// failed, and nothing already running is retroactively adopted. Without a sweep
// here those orphans survive until the host reboots.
//
// They are not inert. Every orphan keeps its Cloudflare edge connections and
// re-registers them for the SAME tunnel, and past a certain count the edge
// starts refusing: mfl-win-amd64-102 accumulated 25 orphans holding 96 edge
// connections and logged "unknown error registering the connection" ~300 times
// an hour for three days, while the live daemon ran on 3 of its 4 connections.
// Reaping them dropped that to ~0 immediately. Cloudflare load-balances across
// every connection registered for a tunnel, so a webhook handed to an orphan is
// a webhook the live daemon never sees.
//
// MATCHING IS BY IMAGE PATH, not by name. Only processes running the binary
// under THIS ephemerd's data dir are touched, so an unrelated cloudflared the
// operator runs for their own tunnel is left alone. Anything already parented
// to this process is skipped — on the restart path our own child may exist.
func reapStrayCloudflared(binary string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	want, err := filepath.Abs(binary)
	if err != nil {
		log.Debug("reaping strays: resolving binary path", "error", err)
		return
	}
	want = strings.ToLower(want)
	self := uint32(os.Getpid())

	pids, err := enumProcesses()
	if err != nil {
		log.Debug("reaping strays: enumerating processes", "error", err)
		return
	}

	var killed int
	for _, pid := range pids {
		if pid == 0 || pid == self {
			continue
		}
		path, ok := processImagePath(pid)
		if !ok || strings.ToLower(path) != want {
			continue
		}
		if parentPID(pid) == self {
			continue // our own child, not a stray
		}
		if err := killPID(pid); err != nil {
			log.Warn("could not kill stray cloudflared", "pid", pid, "error", err)
			continue
		}
		killed++
	}
	if killed > 0 {
		// INFO, not Debug: this is a leak being cleaned up, and the count is
		// the only signal that something upstream is still leaking.
		log.Info("killed stray cloudflared processes left by a previous ephemerd",
			"count", killed, "binary", want)
	}
}

func enumProcesses() ([]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()

	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(snap, &e); err != nil {
		return nil, err
	}
	var out []uint32
	for {
		out = append(out, e.ProcessID)
		if err := windows.Process32Next(snap, &e); err != nil {
			return out, nil //nolint:nilerr // ERROR_NO_MORE_FILES ends the walk
		}
	}
}

func parentPID(pid uint32) uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer func() { _ = windows.CloseHandle(snap) }()

	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(snap, &e); err != nil {
		return 0
	}
	for {
		if e.ProcessID == pid {
			return e.ParentProcessID
		}
		if err := windows.Process32Next(snap, &e); err != nil {
			return 0
		}
	}
}

// processImagePath returns a PID's full executable path. Opening with
// QUERY_LIMITED_INFORMATION is deliberate: it succeeds across sessions and for
// processes we do not own, which QUERY_INFORMATION does not.
func processImagePath(pid uint32) (string, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", false
	}
	defer func() { _ = windows.CloseHandle(h) }()

	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", false
	}
	return windows.UTF16ToString(buf[:n]), true
}

func killPID(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return windows.TerminateProcess(h, 1)
}
