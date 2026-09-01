package scheduler

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/ephpm/ephemerd/pkg/runnerbusy"
	"github.com/ephpm/ephemerd/pkg/vm"
	"golang.org/x/crypto/ssh"
)

// macOSVMBusy reports whether the runner inside a per-job macOS VM is
// executing a job.
//
// A macOS job runs the actions-runner as an ordinary process inside a
// guest VM, so neither the /proc walk (Linux containers) nor HCS (Windows
// containers) applies: from the host, a Virtualization.framework guest is
// one opaque process. The guest is however already reachable over SSH on
// the ephemeral key ephemerd generates at startup — that is how the job
// is set up and how `ephemerd ssh` attaches — so the same channel answers
// the process question. pgrep's exit status is the whole answer; nothing
// is parsed out of the guest's stdout.
//
// Every failure to reach or interrogate the guest is Unknown, never Idle.
func (s *Scheduler) macOSVMBusy(ctx context.Context, mv vm.MacOSVM) (runnerbusy.State, error) {
	ip := mv.RunnerAddress()
	if ip == "" {
		return runnerbusy.Unknown, errors.New("macOS VM has no discovered address yet")
	}

	s.mu.Lock()
	cfg := s.cfg.MacOSVMConfig
	s.mu.Unlock()
	if cfg == nil {
		return runnerbusy.Unknown, errors.New("macOS VM config is not set")
	}

	var auth []ssh.AuthMethod
	if key, ok := cfg.SSHSigner.(ed25519.PrivateKey); ok {
		signer, err := ssh.NewSignerFromKey(key)
		if err != nil {
			return runnerbusy.Unknown, fmt.Errorf("building SSH signer for the guest: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if len(auth) == 0 {
		return runnerbusy.Unknown, errors.New("no SSH key available for the guest")
	}

	deadline, ok := ctx.Deadline()
	timeout := 5 * time.Second
	if ok {
		if d := time.Until(deadline); d > 0 && d < timeout {
			timeout = d
		}
	}

	// The guest is an ephemeral, per-job VM on a host-only network whose
	// host key is regenerated with the clone, so there is no key to pin —
	// same trust model the rest of the macOS VM plumbing uses.
	client, err := ssh.Dial("tcp", net.JoinHostPort(ip, "22"), &ssh.ClientConfig{
		User:            "admin",
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	})
	if err != nil {
		return runnerbusy.Unknown, fmt.Errorf("connecting to the macOS guest at %s: %w", ip, err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			s.cfg.Log.Debug("closing guest SSH session after busy probe", "ip", ip, "error", err)
		}
	}()

	session, err := client.NewSession()
	if err != nil {
		return runnerbusy.Unknown, fmt.Errorf("opening a session on the macOS guest at %s: %w", ip, err)
	}
	defer func() {
		if err := session.Close(); err != nil && !errors.Is(err, io.EOF) {
			s.cfg.Log.Debug("closing guest SSH session after busy probe", "ip", ip, "error", err)
		}
	}()

	// -x demands an exact process-name match, so a build step that merely
	// mentions the worker in its command line cannot fake a busy verdict.
	err = session.Run("pgrep -x Runner.Worker >/dev/null")
	if err == nil {
		return runnerbusy.Busy, nil
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitStatus() == 1 {
		return runnerbusy.Idle, nil
	}
	return runnerbusy.Unknown, fmt.Errorf("probing for a worker on the macOS guest at %s: %w", ip, err)
}
