//go:build linux

package dind

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"
)

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
func startPortForwardProxy(netnsPath, hostIP, hostPort, targetIP, targetPort string) (stop func(), err error) {
	if netnsPath == "" {
		return nil, errors.New("runner netns not set")
	}
	if hostIP == "" {
		hostIP = "127.0.0.1"
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

		// Accept loop runs in the runner's netns. Each forwarded connection
		// dials targetIP:targetPort from THIS namespace too — the kindest/node
		// is on the same CNI bridge, so the runner can reach it directly.
		target := net.JoinHostPort(targetIP, targetPort)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go forwardConn(conn, target)
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

func forwardConn(client net.Conn, target string) {
	defer func() { _ = client.Close() }()

	server, err := net.Dial("tcp", target)
	if err != nil {
		return
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
}
