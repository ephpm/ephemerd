package dind

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// handleContainerAttach implements Docker's POST /containers/{id}/attach.
//
// Docker CLI's `docker run <image>` (non-detached) issues a three-step dance:
//
//	1. POST /containers/create      → returns container ID
//	2. POST /containers/{id}/attach → HTTP/1.1 Upgrade hijacks the conn
//	3. POST /containers/{id}/start  → kicks off the task
//
// The attach step is the one that fails today with "unable to upgrade to tcp,
// received 501" because we previously returned StatusNotImplemented for any
// /attach call. This handler completes the hijack handshake, blocks until
// handleContainerStart signals the task is running, then tails the container's
// log file back through the upgraded connection until the task exits.
//
// Output framing follows Docker's stdcopy convention when the container was
// not started with a TTY: every chunk is prefixed with an 8-byte header
// (stream type + 4-byte big-endian size) so the client can demultiplex stdout
// and stderr. With TTY=true the bytes stream raw. Because containerd's
// cio.LogFile merges stdout and stderr into a single file, we report every
// chunk on stream 1 (stdout) under non-TTY mode; perfect-fidelity stdout/
// stderr split would require splitting the log files at task-create time and
// isn't worth the surgery for the `docker run` flows we need to unblock.
//
// Stdin attach (POST with ?stdin=1) is accepted at the protocol level but
// not piped into the container's task — Docker's stdin-attach semantics
// require an IO mode set at task-create time, which dind doesn't currently
// expose. Clients that care (`docker run -i`) will see a writable hijacked
// conn but their bytes go nowhere. Out of scope for this fix; container
// stdout/stderr capture is what blocks ephpm's `docker run alpine:3.20`.
func (s *Server) handleContainerAttach(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	entry, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", id),
		})
		return
	}

	upgradeHdr := r.Header.Get("Upgrade")
	connHdr := strings.ToLower(r.Header.Get("Connection"))
	wantHijack := upgradeHdr != "" && strings.Contains(connHdr, "upgrade")
	if !wantHijack {
		// Non-hijack attach (clients that send Accept: application/vnd.docker.raw-stream
		// over plain HTTP). Less common; respond with 400 so the client picks
		// a different code path. Docker daemon also accepts this with chunked
		// streaming, but every CLI we care about does the upgrade.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "attach requires HTTP/1.1 Upgrade: tcp (set Upgrade and Connection headers)",
		})
		return
	}

	// Drain the request body before hijacking; same rationale as the exec
	// hijack path — the body bytes sit in the bufio.Reader and would corrupt
	// the upgraded stream if left unread.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		s.log.Debug("draining attach body", "container", id, "error", err)
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "server does not support hijacking",
		})
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("hijack failed: %v", err),
		})
		return
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			s.log.Debug("closing attach conn", "container", id, "error", cerr)
		}
	}()

	upgradeResp := "HTTP/1.1 101 UPGRADED\r\n" +
		"Content-Type: application/vnd.docker.raw-stream\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: tcp\r\n\r\n"
	if _, werr := conn.Write([]byte(upgradeResp)); werr != nil {
		s.log.Debug("writing attach upgrade header", "container", id, "error", werr)
		return
	}

	// Wait for handleContainerStart to signal the task is running (and
	// LogPath is set). The client controls the timing here: it sends
	// /start a few hundred ms after /attach, so 60s is generous. If the
	// caller never sends /start, we bail cleanly when the request context
	// is cancelled.
	if !s.waitForStart(r.Context(), entry, 60*time.Second) {
		// Either the deadline expired or the request was cancelled.
		// Either way nothing useful to stream.
		s.log.Debug("attach: task never started", "container", id)
		return
	}

	// Tail the log file until the task exits. cio.LogFile merges stdout and
	// stderr, so under non-TTY framing we emit everything on stream 1.
	var sink io.Writer
	if entry.Tty {
		sink = conn
	} else {
		mu := &sync.Mutex{}
		sink = &stdcopyWriter{mu: mu, w: conn, streamType: 1}
	}

	ctx := namespaces.WithNamespace(context.Background(), s.jobNamespace)
	done := make(chan struct{})
	if entry.Task != nil {
		ch, werr := entry.Task.Wait(ctx)
		if werr != nil {
			s.log.Debug("attach: task.Wait failed", "container", id, "error", werr)
			// No status channel — close immediately so the tail loop
			// doesn't wait forever. The client will see whatever has
			// already been written to the log.
			close(done)
		} else {
			// Bridge containerd's ExitStatus channel onto a plain done
			// signal so tailLogToWriter doesn't depend on containerd
			// types (keeps it unit-testable).
			go func() {
				<-ch
				close(done)
			}()
		}
	} else {
		close(done)
	}

	s.tailLogToWriter(entry.LogPath, sink, done)
}

// waitForStart blocks until entry.started is closed (i.e. handleContainerStart
// finished task.Start successfully), returning true. If the request is
// cancelled or the deadline expires first, returns false. Idempotent and
// safe to call after the channel is already closed.
func (s *Server) waitForStart(reqCtx context.Context, entry *containerEntry, deadline time.Duration) bool {
	if entry.started == nil {
		// Defensive: a container created before this field was wired in. We
		// can still infer "started" from entry.Task being non-nil — fall
		// back to a polling loop with the same deadline.
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		expiry := time.Now().Add(deadline)
		for {
			if entry.Task != nil && entry.LogPath != "" {
				return true
			}
			select {
			case <-reqCtx.Done():
				return false
			case <-t.C:
				if time.Now().After(expiry) {
					return false
				}
			}
		}
	}
	select {
	case <-entry.started:
		return true
	case <-reqCtx.Done():
		return false
	case <-time.After(deadline):
		return false
	}
}

// tailLogToWriter reads logPath as it grows and streams new bytes to dst
// until either done is closed (task has exited or the caller gave up) or a
// write to dst fails (client disconnected). On hot poll the loop sleeps
// briefly between EOF retries; on actual data it forwards as fast as the
// source produces.
//
// dst is the framed writer (stdcopyWriter for non-TTY, raw conn for TTY).
// The dst writer is the only signal we use for "client gone" — a failed
// write returns and the caller closes the conn via defer.
func (s *Server) tailLogToWriter(logPath string, dst io.Writer, done <-chan struct{}) {
	// Open the log file. handleContainerStart created it before task.Start
	// returned, so by the time waitForStart unblocks, this open should
	// succeed immediately.
	f, err := os.Open(logPath)
	if err != nil {
		s.log.Debug("attach: opening log file", "path", logPath, "error", err)
		return
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			s.log.Debug("attach: closing log file", "path", logPath, "error", cerr)
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		// Drain whatever is currently available.
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					// Client gone or upgraded conn closed.
					s.log.Debug("attach: writing to client", "error", werr)
					return
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				s.log.Debug("attach: reading log file", "error", rerr)
				return
			}
		}
		// At EOF — wait for either more data, task exit, or client disconnect.
		select {
		case <-done:
			// Task exited. Drain any final bytes before closing.
			for {
				n, rerr := f.Read(buf)
				if n > 0 {
					if _, werr := dst.Write(buf[:n]); werr != nil {
						return
					}
				}
				if rerr == io.EOF || rerr != nil {
					return
				}
			}
		case <-time.After(100 * time.Millisecond):
			// Poll again. (No fsnotify; portable to Windows-host VM and
			// Linux native without extra deps.)
		}
	}
}
