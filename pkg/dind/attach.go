package dind

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// handleContainerAttach implements POST /containers/{id}/attach.
//
// Docker clients (including buildx's docker-container driver) send this with
// Upgrade: tcp + Connection: Upgrade to get a hijacked TCP stream of the
// container's stdio. We respond 101 + multiplexed-stream and tail the
// container's captured log into the connection.
//
// Limitations: we can't inject stdin after task creation (containerd's cio
// is bound at NewTask time, and handleContainerStart uses cio.LogFile).
// This is good enough for buildx — it uses attach as a passive "watch
// container output until it's ready" probe, not for interactive stdin.
func (s *Server) handleContainerAttach(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	entry, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": "container " + id + " not found",
		})
		return
	}

	if entry.LogPath == "" {
		// Container hasn't been started — nothing to attach to yet. Docker
		// returns 400 in this case; matching that keeps clients from
		// endlessly retrying a hijacked connect.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "container not started",
		})
		return
	}

	if !wantsHijack(r) {
		// Non-hijacked clients get the log contents as-is (Docker does the
		// same with ?stream=0). Content-Type is the raw stream type.
		w.Header().Set("Content-Type", contentTypeRawStream)
		w.WriteHeader(http.StatusOK)
		if data, err := os.ReadFile(entry.LogPath); err == nil {
			_, _ = w.Write(data)
		}
		return
	}

	conn, _, err := hijackConn(w, contentTypeMuxStream)
	if err != nil {
		s.log.Error("attach hijack failed", "container", id, "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	s.log.Info("container attached (hijacked)", "container", id)

	mux := newStreamMux(conn)
	out := &streamMuxWriter{mux: mux, stream: stdcopyStdout}

	ctx := namespaces.WithNamespace(context.Background(), s.jobNamespace)

	// Tail the log file until the container stops or the client disconnects.
	// Log file writes from cio happen continuously as the task runs; we
	// poll rather than use inotify to keep this portable across overlay/
	// SMB mount paths that might not support notifications.
	f, err := os.Open(entry.LogPath)
	if err != nil {
		s.log.Debug("opening attach log", "container", id, "error", err)
		return
	}
	defer func() { _ = f.Close() }()

	reqCtx := r.Context()
	buf := make([]byte, 4096)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				s.log.Debug("attach write to conn", "error", werr)
				return
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			s.log.Debug("attach log read", "error", readErr)
			return
		}
		if readErr == nil {
			continue // more data available right now
		}

		// At EOF — check if the task is still running before sleeping.
		running := false
		if entry.Task != nil {
			if st, statusErr := entry.Task.Status(ctx); statusErr == nil {
				running = st.Status == client.Running
			}
		}
		if !running {
			// One final read in case the cio writer drained a last chunk
			// between EOF and the Status check.
			if n, _ := f.Read(buf); n > 0 {
				_, _ = out.Write(buf[:n])
			}
			return
		}

		select {
		case <-reqCtx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}
