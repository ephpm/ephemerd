package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/upgrade"

	"github.com/urfave/cli/v3"
)

// upgradeCmd drives the daemon's Upgrade RPC: it tells the running ephemerd
// to fetch a specific release, verify it, drain, swap its own binary, and
// restart. This is a thin client over the control socket — the daemon does
// the work over its OWN outbound HTTPS, so there is no exec-channel size or
// timeout limit. After the daemon reports RESTARTING, this command polls
// Status until the new version is live.
func upgradeCmd() *cli.Command {
	return &cli.Command{
		Name:  "upgrade",
		Usage: "Download and install a specific ephemerd release, then restart the service",
		Description: "Tells the running daemon to upgrade itself to an explicit version. The daemon\n" +
			"downloads the release asset over its own HTTPS, checksum-verifies it, drains\n" +
			"running jobs, swaps its binary (keeping the old one as .old for rollback), and\n" +
			"restarts. A version is REQUIRED — ephemerd never resolves \"latest\" on its own.\n" +
			"\n" +
			"    ephemerd upgrade --version v0.1.7",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "version",
				Usage:    "target release tag to install (e.g. v0.1.7) — REQUIRED",
				Required: true,
			},
			&cli.StringFlag{
				Name:  "url",
				Usage: "override the release base URL (for testing / air-gapped mirrors); asset and checksums.txt are fetched from <url>/",
			},
			&cli.BoolFlag{
				Name:  "no-drain",
				Usage: "skip draining running jobs before the swap (interrupts in-flight jobs)",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "re-install even if the daemon already reports the target version",
			},
			&cli.DurationFlag{
				Name:  "drain-timeout",
				Value: 45 * time.Minute,
				Usage: "give up if running jobs don't finish within this long (upgrade aborts, node stays on old binary)",
			},
			&cli.DurationFlag{
				Name: "restart-timeout",
				// Longer than the daemon's own restart supervision (~3m: two
				// attempts behind a 90s watchdog each), so by the time this
				// gives up the daemon has already decided whether the restart
				// landed and has un-cordoned itself if it did not. That makes
				// the "accepting jobs" line in the failure message true rather
				// than a snapshot taken mid-decision.
				Value: 5 * time.Minute,
				Usage: "how long to wait after RESTARTING for the new version to come up",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			target := cmd.String("version")
			if cmd.String("url") == "" && !upgrade.ValidVersion(target) {
				return fmt.Errorf("--version %q is not a release tag of the form vX.Y.Z", target)
			}
			return runUpgrade(ctx, upgradeArgs{
				target:         target,
				url:            cmd.String("url"),
				noDrain:        cmd.Bool("no-drain"),
				force:          cmd.Bool("force"),
				drainTimeout:   cmd.Duration("drain-timeout"),
				restartTimeout: cmd.Duration("restart-timeout"),
			})
		},
	}
}

type upgradeArgs struct {
	target         string
	url            string
	noDrain        bool
	force          bool
	drainTimeout   time.Duration
	restartTimeout time.Duration
}

func runUpgrade(ctx context.Context, args upgradeArgs) error {
	cc, err := dialControl(ctx)
	if err != nil {
		return err
	}

	stream, err := cc.Upgrade(ctx, &apiv1.UpgradeRequest{
		TargetVersion:       args.target,
		UrlOverride:         args.url,
		NoDrain:             args.noDrain,
		Force:               args.force,
		DrainTimeoutSeconds: int32(args.drainTimeout.Seconds()),
	})
	if err != nil {
		_ = cc.Close()
		return fmt.Errorf("starting upgrade: %w", err)
	}

	var restarting bool
	for {
		p, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			// After RESTARTING the daemon tears down; a stream error here is
			// expected, not an upgrade failure — fall through to polling.
			if restarting {
				break
			}
			_ = cc.Close()
			return fmt.Errorf("upgrade: %w", recvErr)
		}
		printUpgradeProgress(p)
		switch p.State {
		case apiv1.UpgradeState_UPGRADE_STATE_UP_TO_DATE:
			_ = cc.Close()
			return nil
		case apiv1.UpgradeState_UPGRADE_STATE_RESTARTING:
			restarting = true
		case apiv1.UpgradeState_UPGRADE_STATE_FAILED:
			_ = cc.Close()
			return fmt.Errorf("upgrade failed: %s", p.Message)
		}
	}
	_ = cc.Close()

	if !restarting {
		// Stream ended without a RESTARTING message and without UP_TO_DATE:
		// the daemon returned before swapping. Nothing more to poll.
		return nil
	}

	fmt.Printf("Service is restarting into %s; waiting for it to come up...\n", args.target)
	return pollForVersion(ctx, args.target, args.restartTimeout)
}

// pollForVersion reconnects to the control socket and polls Status until the
// daemon reports the target version or the timeout elapses.
func pollForVersion(ctx context.Context, target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var last *apiv1.StatusResponse
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			cc, err := dialControl(ctx)
			if err != nil {
				if time.Now().After(deadline) {
					return incompleteUpgradeError(target, timeout, nil, err)
				}
				continue
			}
			resp, err := cc.Status(ctx, &apiv1.StatusRequest{})
			_ = cc.Close()
			if err == nil {
				last = resp
				if upgrade.SameVersion(resp.Version, target) {
					fmt.Printf("Upgraded: daemon is running %s.\n", resp.Version)
					return nil
				}
			}
			if time.Now().After(deadline) {
				return incompleteUpgradeError(target, timeout, last, err)
			}
		}
	}
}

// incompleteUpgradeError spells out the state the node is actually in when
// the restart never lands, because the previous message ("timed out ...
// daemon reports v0.1.7") described the symptom and left the two facts that
// matter unsaid: the binary on disk WAS replaced, and the node may or may not
// still be taking jobs.
//
// It deliberately does NOT auto-roll-back. The new binary is the one thing
// here that has been proven good — checksum-verified and `--version`-probed
// before the swap — while the thing that failed is the service-manager
// hand-off, which a rollback does not fix. Worse, rolling back races a
// restart that may still be in flight: on the v0.1.8 incident the SCM stop
// landed 6m34s after the swap, so an auto-rollback at the 3m mark would have
// undone a restart that was about to succeed and left the node running old
// code with a newer .old beside it. Leaving the verified binary staged means
// the very next restart — automatic or manual — completes the upgrade. What
// must never persist is the cordon, and the daemon now clears that itself.
func incompleteUpgradeError(target string, timeout time.Duration, last *apiv1.StatusResponse, dialErr error) error {
	running := "unreachable"
	accepting := "UNKNOWN — the daemon is not answering the control socket"
	if last != nil {
		if last.Version != "" {
			running = last.Version
		} else {
			running = "unknown"
		}
		if last.Draining {
			accepting = "NO — the node is DRAINED and still running the old binary"
		} else {
			accepting = "yes — the node is accepting jobs on the old binary"
		}
	}
	msg := fmt.Sprintf(`upgrade INCOMPLETE after %s

  %s IS installed on disk (the previous binary was kept beside it as ephemerd.exe.old),
  but the service did NOT restart into it.

  daemon reports:  %s
  accepting jobs:  %s

  finish the upgrade:  %s
  or roll back:        stop the service, restore the .old binary over the installed one, start it
  then verify:         ephemerd status   (and ephemerd logs for why the restart stalled)`,
		timeout, target, running, accepting, upgrade.ManualRestartHint())
	if dialErr != nil && last == nil {
		msg += fmt.Sprintf("\n\n  last control-socket error: %v", dialErr)
	}
	return errors.New(msg)
}

func printUpgradeProgress(p *apiv1.UpgradeProgress) {
	label := upgradeStateLabel(p.State)
	switch {
	case p.State == apiv1.UpgradeState_UPGRADE_STATE_DOWNLOADING && p.BytesTotal > 0:
		pct := float64(p.BytesDownloaded) / float64(p.BytesTotal) * 100
		fmt.Printf("[%s] %s (%.0f%%, %d/%d bytes)\n", label, p.Message, pct, p.BytesDownloaded, p.BytesTotal)
	case p.State == apiv1.UpgradeState_UPGRADE_STATE_DOWNLOADING && p.BytesDownloaded > 0:
		fmt.Printf("[%s] %s (%d bytes)\n", label, p.Message, p.BytesDownloaded)
	default:
		fmt.Printf("[%s] %s\n", label, p.Message)
	}
}

func upgradeStateLabel(s apiv1.UpgradeState) string {
	switch s {
	case apiv1.UpgradeState_UPGRADE_STATE_PREFLIGHT:
		return "preflight"
	case apiv1.UpgradeState_UPGRADE_STATE_UP_TO_DATE:
		return "up-to-date"
	case apiv1.UpgradeState_UPGRADE_STATE_DRAINING:
		return "draining"
	case apiv1.UpgradeState_UPGRADE_STATE_DOWNLOADING:
		return "downloading"
	case apiv1.UpgradeState_UPGRADE_STATE_VERIFYING:
		return "verifying"
	case apiv1.UpgradeState_UPGRADE_STATE_STAGING:
		return "staging"
	case apiv1.UpgradeState_UPGRADE_STATE_SWAPPING:
		return "swapping"
	case apiv1.UpgradeState_UPGRADE_STATE_RESTARTING:
		return "restarting"
	case apiv1.UpgradeState_UPGRADE_STATE_FAILED:
		return "failed"
	default:
		return "unknown"
	}
}
