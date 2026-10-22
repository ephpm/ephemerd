//go:build linux

package dind

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// forwardDialTimeout bounds the dial to the container behind a published
// port. Without it a dial whose SYN is never answered blocks for the kernel's
// ~2 minute connect timeout while the client, already accepted on 127.0.0.1,
// sees an open connection that never sends a byte. A var so tests can
// shorten it.
var forwardDialTimeout = 5 * time.Second

// startPortForwardProxy starts a userspace TCP proxy that listens on
// hostIP:hostPort *inside the runner container's net namespace* and forwards
// every accepted connection to targetIP:targetPort (reachable from main ns
// since it sits on the same CNI bridge).
//
// We pin a goroutine to an OS thread, setns it into the runner's netns, and
// run net.Listen there — once the socket is created it stays bound to that
// namespace, so subsequent Accepts come from the runner's loopback.
//
// This is the userspace analog of `docker -p 127.0.0.1:hostPort:targetPort`.
// We use a Go proxy instead of iptables DNAT because iptables-nft inside the
// runner image needs nft kernel modules (xt_DNAT, nft_nat) that aren't all
// reliably available in our minimal VM kernel — userspace forwarding has no
// such constraints and is portable across legacy/nft-only images.
//
// Returns a stop function that closes the listener (idempotent), or an error
// if setup fails.
func startPortForwardProxy(netnsPath, hostIP, hostPort, targetIP, targetPort string, log *slog.Logger) (stop func(), err error) {
	if netnsPath == "" {
		return nil, errors.New("runner netns not set")
	}
	if hostIP == "" {
		hostIP = "127.0.0.1"
	}
	if log == nil {
		log = slog.Default()
	}

	// Channel to receive (listener, error) from the namespace-locked goroutine.
	type listenResult struct {
		listener net.Listener
		err      error
	}
	resultCh := make(chan listenResult, 1)

	// closeOnce is shared with the accept loop so stop() is idempotent.
	var closeOnce sync.Once

	go func() {
		// Lock the goroutine to an OS thread so setns persists for its lifetime.
		// We never unlock — when this goroutine returns, Go discards the thread.
		runtime.LockOSThread()

		// Save current netns fd so we can return on failure.
		f, err := os.Open(netnsPath)
		if err != nil {
			resultCh <- listenResult{err: fmt.Errorf("open netns: %w", err)}
			return
		}
		defer func() { _ = f.Close() }()

		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			resultCh <- listenResult{err: fmt.Errorf("setns: %w", err)}
			return
		}

		listener, err := net.Listen("tcp", net.JoinHostPort(hostIP, hostPort))
		if err != nil {
			resultCh <- listenResult{err: fmt.Errorf("listen: %w", err)}
			return
		}
		resultCh <- listenResult{listener: listener}

		// Accept loop runs in the runner's netns. Each accepted connection is
		// handed to a new goroutine, which is NOT locked to this thread, so
		// the dial to targetIP:targetPort goes out from the daemon's own
		// namespace — the container sits on the CNI bridge the daemon owns,
		// so it is reachable from there.
		target := net.JoinHostPort(targetIP, targetPort)
		listen := net.JoinHostPort(hostIP, hostPort)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				if err := forwardConn(conn, target); err != nil {
					log.Warn("port forward: cannot reach container; closed the client connection",
						"listen", listen, "target", target, "error", err)
				}
			}()
		}
	}()

	res := <-resultCh
	if res.err != nil {
		return nil, res.err
	}

	stop = func() {
		closeOnce.Do(func() {
			if res.listener != nil {
				_ = res.listener.Close()
			}
		})
	}
	return stop, nil
}

// forwardConn relays client to target until either side closes. If target
// cannot be dialed within forwardDialTimeout it closes client and returns the
// dial error, so the client sees the failure at once instead of an open
// connection that never answers.
func forwardConn(client net.Conn, target string) error {
	defer func() { _ = client.Close() }()

	server, err := net.DialTimeout("tcp", target, forwardDialTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(server, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, server)
		done <- struct{}{}
	}()
	<-done
	return nil
}
