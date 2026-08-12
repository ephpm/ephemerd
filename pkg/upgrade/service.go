package upgrade

import "time"

// RestartHelperCommand is argv[1] of the hidden CLI command that performs a
// service restart on behalf of a daemon that is about to be stopped.
//
// It lives here rather than in cmd/ephemerd because both sides need it: this
// package spawns `<ephemerd> __restart-service` (see triggerRestart on
// Windows) and cmd/ephemerd registers the command that answers to it. The
// double underscore marks it as internal plumbing; it is hidden from help.
const RestartHelperCommand = "__restart-service"

// Default bounds for RestartService. The stop budget is deliberately larger
// than the daemon's own SCM stop backstop (6m in cmd/ephemerd/svc_windows.go)
// so the helper never gives up on a stop that is still legitimately draining.
const (
	DefaultRestartStopTimeout  = 8 * time.Minute
	DefaultRestartStartTimeout = 2 * time.Minute
	defaultRestartPoll         = 500 * time.Millisecond
)

// RestartOptions bounds a service-manager restart. Zero fields take the
// Default* values above.
type RestartOptions struct {
	StopTimeout  time.Duration
	StartTimeout time.Duration
	Poll         time.Duration
}

func (o RestartOptions) withDefaults() RestartOptions {
	if o.StopTimeout <= 0 {
		o.StopTimeout = DefaultRestartStopTimeout
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = DefaultRestartStartTimeout
	}
	if o.Poll <= 0 {
		o.Poll = defaultRestartPoll
	}
	return o
}

// ManualRestartHint is the command an operator runs to restart the service by
// hand on this platform. Used in the failure messages an upgrade emits when
// the automatic restart does not take, so the remediation is never a guess.
func ManualRestartHint() string { return manualRestartHint }
