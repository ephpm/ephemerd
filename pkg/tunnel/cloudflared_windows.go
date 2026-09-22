//go:build windows

package tunnel

import (
	"fmt"
	"io"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// applyPdeathsig is a no-op on Windows: SysProcAttr has no Pdeathsig field.
// bindChildLifetime below provides the equivalent guarantee.
func applyPdeathsig(_ *exec.Cmd) {}

// bindChildLifetime puts cloudflared in a Job Object marked
// KILL_ON_JOB_CLOSE, so the kernel terminates it when the last handle to the
// job goes away — which happens automatically when ephemerd's process exits,
// by any means.
//
// This is the Windows counterpart to Pdeathsig on Linux, and it exists because
// Windows had NOTHING. The old comment claimed "the graceful Close() path is
// the sole shutdown mechanism there", but that path signals SIGTERM, and Go's
// os.Process.Signal rejects every signal except os.Kill on Windows. The error
// was logged at Debug and dropped. So a service stop — every `mayfly apply`,
// every `node upgrade` — killed ephemerd without running cleanup and left the
// tunnel client running.
//
// Measured on mfl-win-amd64-102 2026-09-22: 25 orphaned cloudflared processes,
// oldest 13 days old, 868 MB resident, holding 96 established connections to
// Cloudflare's edge against 4 for the live daemon. They all share one tunnel
// config, and Cloudflare load-balances across every connection registered for
// a tunnel, so the overwhelming majority of inbound webhook deliveries were
// being handed to clients whose ephemerd was gone.
//
// The returned Closer must be held for as long as the child should live:
// closing it is itself a guaranteed kill, which is why close() uses it as the
// backstop after the graceful path.
func bindChildLifetime(cmd *exec.Cmd) (io.Closer, error) {
	if cmd.Process == nil {
		return nil, fmt.Errorf("cloudflared: bind lifetime: process not started")
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("cloudflared: create job object: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("cloudflared: set job limits: %w", err)
	}

	// OpenProcess rather than reusing the handle inside os.Process: that one is
	// owned by the runtime and closed when the Process is released, which would
	// make assignment racy against Wait.
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("cloudflared: open child process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()

	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("cloudflared: assign to job object: %w", err)
	}
	return &jobHandle{h: job}, nil
}

// jobHandle closes the Job Object, which kills everything still in it.
type jobHandle struct{ h windows.Handle }

func (j *jobHandle) Close() error {
	if j == nil || j.h == 0 {
		return nil
	}
	h := j.h
	j.h = 0
	return windows.CloseHandle(h)
}
