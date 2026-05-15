package dind

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// execEntry tracks an exec process inside a running container.
//
// The containerd Process is not created at exec-create time — `cio` (stdin/stdout/stderr)
// is fixed at process creation, and we don't yet know whether the start request will be
// a hijacked stream (`docker exec -i` / buildx) or a synchronous run-and-buffer (`act`).
// So we defer Task.Exec to handleExecStart where we know which IO mode to wire up.
type execEntry struct {
	ID          string
	ContainerID string
	Cmd         []string
	Env         []string
	WorkingDir  string
	Tty         bool
	AttachStdin bool

	Process  client.Process
	LogPath  string
	Running  bool
	ExitCode int
	exited   bool
}

type execCreateRequest struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env"`
	WorkingDir   string   `json:"WorkingDir"`
}

// routeExec dispatches /exec/{id}/{action} requests.
func (s *Server) routeExec(w http.ResponseWriter, r *http.Request, path string) {
	rest := strings.TrimPrefix(path, "/exec/")
	parts := strings.SplitN(rest, "/", 2)
	execID := parts[0]

	if len(parts) == 1 {
		s.handleNotImplemented(w, r)
		return
	}

	action := parts[1]
	switch {
	case action == "start" && r.Method == http.MethodPost:
		s.handleExecStart(w, r, execID)
	case action == "json" && r.Method == http.MethodGet:
		s.handleExecInspect(w, r, execID)
	default:
		s.handleNotImplemented(w, r)
	}
}

func (s *Server) handleExecCreate(w http.ResponseWriter, r *http.Request, containerID string) {
	s.mu.Lock()
	entry, ok := s.containers[containerID]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", containerID),
		})
		return
	}

	if entry.Task == nil || entry.Status != "running" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"message": fmt.Sprintf("container %s is not running", containerID),
		})
		return
	}

	var req execCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": fmt.Sprintf("invalid request body: %v", err),
		})
		return
	}

	if len(req.Cmd) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "Cmd is required",
		})
		return
	}

	execID := generateContainerID()[:16]

	cwd := req.WorkingDir
	if cwd == "" {
		cwd = "/"
	}

	// Inherit container env, then overlay exec-specific env. Copy the slice so
	// we don't mutate the container spec's underlying array.
	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)
	var env []string
	if spec, err := entry.Container.Spec(ctx); err == nil && spec.Process != nil {
		env = append(env, spec.Process.Env...)
	}
	env = append(env, req.Env...)

	exec := &execEntry{
		ID:          execID,
		ContainerID: containerID,
		Cmd:         req.Cmd,
		Env:         env,
		WorkingDir:  cwd,
		Tty:         req.Tty,
		AttachStdin: req.AttachStdin,
	}

	s.mu.Lock()
	s.execs[execID] = exec
	s.mu.Unlock()

	s.log.Info("exec created", "exec_id", execID, "container", containerID, "cmd", req.Cmd)

	writeJSON(w, http.StatusCreated, map[string]any{
		"Id": execID,
	})
}

func (s *Server) handleExecStart(w http.ResponseWriter, r *http.Request, execID string) {
	s.mu.Lock()
	exec, ok := s.execs[execID]
	var entry *containerEntry
	if ok {
		entry = s.containers[exec.ContainerID]
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("exec %s not found", execID),
		})
		return
	}
	if exec.Running || exec.exited {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if entry == nil || entry.Task == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "container task not available",
		})
		return
	}

	// Detect HTTP/1.1 hijack request (`docker exec -i` and friends — used by
	// buildx's docker-container driver to talk to buildkitd over stdio).
	upgradeHdr := r.Header.Get("Upgrade")
	connHdr := strings.ToLower(r.Header.Get("Connection"))
	wantHijack := upgradeHdr != "" && strings.Contains(connHdr, "upgrade")
	if wantHijack {
		s.handleExecStartHijack(w, r, exec, entry)
		return
	}

	s.handleExecStartLog(w, r, exec, entry)
}

// handleExecStartLog runs the exec with output captured to a log file and
// returns the full output once the process exits. This is the synchronous
// path used by clients that don't request a stream upgrade (e.g. act).
func (s *Server) handleExecStartLog(w http.ResponseWriter, r *http.Request, exec *execEntry, entry *containerEntry) {
	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)

	pspec := &ocispec.Process{
		Args: exec.Cmd,
		Cwd:  exec.WorkingDir,
		Env:  exec.Env,
		User: ocispec.User{UID: 0, GID: 0},
	}

	logDir := filepath.Join(filepath.Dir(s.sockPath), "exec", exec.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("creating exec log dir: %v", err),
		})
		return
	}
	logPath := filepath.Join(logDir, "output.log")
	exec.LogPath = logPath

	proc, err := entry.Task.Exec(ctx, exec.ID, pspec, cio.LogFile(logPath))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("creating exec: %v", err),
		})
		return
	}
	exec.Process = proc

	statusCh, err := proc.Wait(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("waiting for exec: %v", err),
		})
		return
	}

	if err := proc.Start(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("starting exec: %v", err),
		})
		return
	}

	exec.Running = true
	s.log.Info("exec started (log mode)", "exec_id", exec.ID)

	select {
	case status := <-statusCh:
		if procIO := proc.IO(); procIO != nil {
			procIO.Wait()
		}
		exec.ExitCode = int(status.ExitCode())
		exec.Running = false
		exec.exited = true
	case <-r.Context().Done():
		exec.Running = false
		writeJSON(w, http.StatusRequestTimeout, map[string]string{
			"message": "exec timed out",
		})
		return
	}

	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(http.StatusOK)
	if data, err := os.ReadFile(logPath); err == nil {
		if _, werr := w.Write(data); werr != nil {
			s.log.Debug("writing exec output", "error", werr)
		}
	}
}

// handleExecStartHijack is the path used by `docker exec -i`. It hijacks the
// HTTP/1.1 connection, writes the 101 Switching Protocols response, then
// bridges raw bytes between the connection and the exec process's stdio.
//
// With Tty=false, stdout/stderr are multiplexed using Docker's stdcopy
// framing (8-byte header per chunk) so the client can demultiplex them.
// With Tty=true, all output is raw on a single stream.
func (s *Server) handleExecStartHijack(w http.ResponseWriter, r *http.Request, exec *execEntry, entry *containerEntry) {
	// Drain the request body before hijacking. The body (e.g. {"Detach":false,"Tty":false})
	// sits in the bufio.Reader's buffer after header parsing. If we hijack without
	// reading it, those bytes leak into the upgraded stream and corrupt exec stdin.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		s.log.Warn("draining exec start body", "exec_id", exec.ID, "error", err)
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "server does not support hijacking",
		})
		return
	}
	conn, brw, err := hijacker.Hijack()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("hijack failed: %v", err),
		})
		return
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			s.log.Debug("closing hijacked conn", "exec_id", exec.ID, "error", cerr)
		}
	}()

	upgradeResp := "HTTP/1.1 101 UPGRADED\r\n" +
		"Content-Type: application/vnd.docker.raw-stream\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: tcp\r\n\r\n"
	if _, werr := conn.Write([]byte(upgradeResp)); werr != nil {
		s.log.Warn("writing upgrade header", "exec_id", exec.ID, "error", werr)
		return
	}

	// Buffer all client stdin data upfront with a short deadline. The Docker CLI
	// writes stdin data immediately after receiving the 101 and calls CloseWrite.
	// Using a deadline avoids hanging forever if the client never sends EOF.
	var stdinBuf bytes.Buffer
	var stdinStreaming bool
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		s.log.Debug("setting read deadline", "exec_id", exec.ID, "error", err)
	}
	if _, err := io.Copy(&stdinBuf, brw.Reader); err != nil {
		if os.IsTimeout(err) {
			stdinStreaming = true
		} else {
			s.log.Debug("reading exec stdin", "exec_id", exec.ID, "error", err)
		}
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		s.log.Debug("clearing read deadline", "exec_id", exec.ID, "error", err)
	}
	s.log.Info("exec stdin buffered", "exec_id", exec.ID, "bytes", stdinBuf.Len(), "streaming", stdinStreaming)

	// Fast path: "cp /dev/stdin <path>" is used by KIND to write config files
	// into containers. Containerd's exec FIFO mechanism has a race condition
	// where the shim-side FIFO read end may not be fully connected to the exec
	// process in time, causing the process to hang. Bypass FIFOs entirely by
	// writing directly to the container's overlayfs.
	if s.isCpStdinExec(exec.Cmd) && stdinBuf.Len() > 0 {
		if s.handleCpStdinDirect(conn, exec, entry, stdinBuf.Bytes()) {
			return
		}
	}

	// For one-shot stdin (client sent data then closed), write to a file in
	// the container's overlayfs and use shell redirection. Skip for streaming
	// execs (e.g. buildctl dial-stdio) where stdin remains open.
	if stdinBuf.Len() > 0 && !stdinStreaming {
		if containerPath, ok := s.writeStdinFile(exec, entry, stdinBuf.Bytes()); ok {
			s.log.Info("exec stdin redirected via file", "exec_id", exec.ID, "path", containerPath, "bytes", stdinBuf.Len())
			origCmd := exec.Cmd
			shScript := fmt.Sprintf(`"$@" < %s; s=$?; rm -f %s; exit $s`, containerPath, containerPath)
			exec.Cmd = append([]string{"sh", "-c", shScript, "_"}, origCmd...)
			stdinBuf.Reset()
		}
	}

	// Wire stdout/stderr writers. Multiplex with stdcopy framing when Tty=false.
	var stdoutW, stderrW io.Writer
	if exec.Tty {
		stdoutW = conn
		stderrW = conn
	} else {
		mu := &sync.Mutex{}
		stdoutW = &stdcopyWriter{mu: mu, w: conn, streamType: 1}
		stderrW = &stdcopyWriter{mu: mu, w: conn, streamType: 2}
	}

	// Use a background context — r.Context() is cancelled when the hijacked
	// HTTP handler returns, but we still need containerd ops to run.
	ctx := namespaces.WithNamespace(context.Background(), s.jobNamespace)

	pspec := &ocispec.Process{
		Args:     exec.Cmd,
		Cwd:      exec.WorkingDir,
		Env:      exec.Env,
		User:     ocispec.User{UID: 0, GID: 0},
		Terminal: exec.Tty,
	}

	var stdinR io.Reader
	var stdinPW *io.PipeWriter
	if stdinStreaming {
		// Streaming exec (e.g. buildctl dial-stdio): the hijacked conn IS the
		// ongoing stdin source. Prepend any initial buffered bytes, then read
		// from the live connection. The FIFO race doesn't apply here because
		// the reader never returns EOF until the client disconnects.
		stdinR = io.MultiReader(bytes.NewReader(stdinBuf.Bytes()), conn)
	} else if stdinBuf.Len() > 0 {
		// One-shot stdin that wasn't handled by the file-redirect path.
		// Use io.Pipe to delay delivery until after Start().
		pr, pw := io.Pipe()
		stdinR = pr
		stdinPW = pw
	}

	proc, err := entry.Task.Exec(ctx, exec.ID, pspec, cio.NewCreator(cio.WithStreams(stdinR, stdoutW, stderrW)))
	if err != nil {
		if stdinPW != nil {
			if cerr := stdinPW.Close(); cerr != nil {
				s.log.Debug("closing exec stdin pipe", "exec_id", exec.ID, "error", cerr)
			}
		}
		s.log.Warn("creating hijacked exec", "exec_id", exec.ID, "error", err)
		return
	}
	exec.Process = proc
	defer func() {
		if _, derr := proc.Delete(ctx, client.WithProcessKill); derr != nil {
			s.log.Debug("deleting hijacked exec", "exec_id", exec.ID, "error", derr)
		}
	}()

	statusCh, err := proc.Wait(ctx)
	if err != nil {
		if stdinPW != nil {
			if cerr := stdinPW.Close(); cerr != nil {
				s.log.Debug("closing exec stdin pipe", "exec_id", exec.ID, "error", cerr)
			}
		}
		s.log.Warn("waiting for hijacked exec", "exec_id", exec.ID, "error", err)
		return
	}

	if err := proc.Start(ctx); err != nil {
		if stdinPW != nil {
			if cerr := stdinPW.Close(); cerr != nil {
				s.log.Debug("closing exec stdin pipe", "exec_id", exec.ID, "error", cerr)
			}
		}
		s.log.Warn("starting hijacked exec", "exec_id", exec.ID, "error", err)
		return
	}

	if stdinPW != nil {
		go func() {
			if _, err := stdinPW.Write(stdinBuf.Bytes()); err != nil {
				s.log.Debug("writing exec stdin pipe", "exec_id", exec.ID, "error", err)
			}
			if cerr := stdinPW.Close(); cerr != nil {
				s.log.Debug("closing exec stdin pipe", "exec_id", exec.ID, "error", cerr)
			}
		}()
	}

	exec.Running = true
	s.log.Info("exec started (hijacked)", "exec_id", exec.ID, "tty", exec.Tty)

	status := <-statusCh
	if procIO := proc.IO(); procIO != nil {
		procIO.Wait()
	}
	exec.ExitCode = int(status.ExitCode())
	exec.Running = false
	exec.exited = true

	s.log.Info("exec finished (hijacked)", "exec_id", exec.ID, "exit_code", exec.ExitCode)
}

// stdcopyWriter frames each write with Docker's 8-byte stream-type header so a
// client can demultiplex stdout (1) and stderr (2) over a single connection.
//
// Frame layout: header[0] = stream type, header[1..4] = 0, header[4..8] = BE size, then payload.
//
// Concurrent stdout/stderr writers share a mutex so a partial header and
// payload from one stream are never split by the other stream's frame.
type stdcopyWriter struct {
	mu         *sync.Mutex
	w          io.Writer
	streamType byte
}

func (s *stdcopyWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hdr [8]byte
	hdr[0] = s.streamType
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(p)))
	if _, err := s.w.Write(hdr[:]); err != nil {
		return 0, err
	}
	return s.w.Write(p)
}

func (s *Server) handleExecInspect(w http.ResponseWriter, r *http.Request, execID string) {
	s.mu.Lock()
	exec, ok := s.execs[execID]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("exec %s not found", execID),
		})
		return
	}

	running := exec.Running
	exitCode := exec.ExitCode
	if exec.Process != nil && running {
		ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)
		if st, err := exec.Process.Status(ctx); err == nil {
			switch st.Status {
			case client.Running:
				running = true
			case client.Stopped:
				running = false
				exitCode = int(st.ExitStatus)
				exec.Running = false
				exec.exited = true
				exec.ExitCode = exitCode
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ID":          exec.ID,
		"Running":     running,
		"ExitCode":    exitCode,
		"ContainerID": exec.ContainerID,
		"ProcessConfig": map[string]any{
			"entrypoint": "",
			"arguments":  exec.Cmd,
		},
	})
}

// handleContainerCopyTo handles PUT /containers/{id}/archive.
// Copies a tar stream into the container's filesystem. Uses exec to run
// tar inside the container, avoiding direct overlay mount manipulation.
func (s *Server) handleContainerCopyTo(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	entry, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", id),
		})
		return
	}

	dstPath := r.URL.Query().Get("path")
	if dstPath == "" {
		dstPath = "/"
	}

	// If the container is running, exec tar inside it — this is how Docker
	// does it too and avoids needing to mount the overlay ourselves.
	if entry.Task != nil && entry.Status == "running" {
		s.copyToViaExec(w, r, entry, dstPath)
		return
	}

	// Container not running — write directly to the snapshot's upperdir.
	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)
	snapshotID := entry.ID + "-snapshot"
	snapshotter := s.client.SnapshotService("overlayfs")
	if snapshotter == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "snapshotter not available",
		})
		return
	}

	mounts, err := snapshotter.Mounts(ctx, snapshotID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("getting snapshot mounts: %v", err),
		})
		return
	}

	// Find the overlay upperdir and extract the tar there.
	upperDir := ""
	for _, m := range mounts {
		for _, opt := range m.Options {
			for _, part := range strings.Split(opt, ",") {
				if strings.HasPrefix(part, "upperdir=") {
					upperDir = strings.TrimPrefix(part, "upperdir=")
				}
			}
		}
	}
	if upperDir == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "cannot find overlay upperdir for snapshot",
		})
		return
	}

	targetDir := filepath.Join(upperDir, filepath.Clean(dstPath))
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("creating target dir: %v", err),
		})
		return
	}

	if err := extractTar(r.Body, targetDir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("extracting archive: %v", err),
		})
		return
	}

	s.log.Info("copied archive into container (via upperdir)", "id", id, "path", dstPath)
	w.WriteHeader(http.StatusOK)
}

// copyToViaExec copies a tar stream into a running container by exec'ing
// tar inside it. This is the same approach Docker uses.
func (s *Server) copyToViaExec(w http.ResponseWriter, r *http.Request, entry *containerEntry, dstPath string) {
	// Write the incoming tar to a temp file, then exec tar inside the
	// container to extract it. We can't pipe directly because containerd's
	// exec API doesn't support stdin streaming via cio.LogFile.
	tmpPath, err := streamToTempFile(r.Body, "dind-copy-*.tar")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": err.Error(),
		})
		return
	}
	defer func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			s.log.Debug("removing temp tar", "path", tmpPath, "error", rmErr)
		}
	}()

	// For the exec approach to work, we need the tar file visible inside
	// the container. Since the container uses overlayfs, we can write the
	// tar to the upperdir and then exec tar inside the container.
	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)
	snapshotID := entry.ID + "-snapshot"
	snapshotter := s.client.SnapshotService("overlayfs")
	if snapshotter == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "snapshotter not available",
		})
		return
	}

	mounts, err := snapshotter.Mounts(ctx, snapshotID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("getting snapshot mounts: %v", err),
		})
		return
	}

	upperDir := ""
	for _, m := range mounts {
		for _, opt := range m.Options {
			for _, part := range strings.Split(opt, ",") {
				if strings.HasPrefix(part, "upperdir=") {
					upperDir = strings.TrimPrefix(part, "upperdir=")
				}
			}
		}
	}
	if upperDir == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "cannot find overlay upperdir",
		})
		return
	}

	// Copy tar into a staging location inside the container's filesystem.
	stagingPath := "/.ephemerd-copy.tar"
	hostStagingPath := filepath.Join(upperDir, stagingPath)
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("reading temp tar: %v", err),
		})
		return
	}
	if err := os.WriteFile(hostStagingPath, data, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("staging tar in container: %v", err),
		})
		return
	}
	defer func() { _ = os.Remove(hostStagingPath) }()

	// Exec tar inside the container to extract.
	execID := generateContainerID()[:16]
	pspec := &ocispec.Process{
		Args: []string{"tar", "xf", stagingPath, "-C", dstPath},
		Cwd:  "/",
		User: ocispec.User{UID: 0, GID: 0},
	}

	logDir := filepath.Join(filepath.Dir(s.sockPath), "exec", execID)
	_ = os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(logDir, "output.log")

	proc, err := entry.Task.Exec(ctx, execID, pspec, cio.LogFile(logPath))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("exec tar: %v", err),
		})
		return
	}
	defer func() {
		_, _ = proc.Delete(ctx, client.WithProcessKill)
		_ = os.RemoveAll(logDir)
	}()

	statusCh, err := proc.Wait(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("waiting for tar exec: %v", err),
		})
		return
	}

	if err := proc.Start(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("starting tar exec: %v", err),
		})
		return
	}

	select {
	case status := <-statusCh:
		if status.ExitCode() != 0 {
			logData, _ := os.ReadFile(logPath)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"message": fmt.Sprintf("tar exited %d: %s", status.ExitCode(), string(logData)),
			})
			return
		}
	case <-r.Context().Done():
		writeJSON(w, http.StatusRequestTimeout, map[string]string{
			"message": "copy timed out",
		})
		return
	}

	s.log.Info("copied archive into container (via exec)", "id", entry.ID, "path", dstPath)
	w.WriteHeader(http.StatusOK)
}

// handleContainerCopyFrom handles GET /containers/{id}/archive.
// Tars up files from the container's rootfs and streams them back.
func (s *Server) handleContainerCopyFrom(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	entry, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", id),
		})
		return
	}

	srcPath := r.URL.Query().Get("path")
	if srcPath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "path query parameter is required",
		})
		return
	}

	// For running containers, exec tar inside to stream files out.
	// For stopped containers, read from the overlay upperdir.
	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)
	snapshotID := entry.ID + "-snapshot"
	snapshotter := s.client.SnapshotService("overlayfs")
	if snapshotter == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "snapshotter not available",
		})
		return
	}

	mounts, err := snapshotter.Mounts(ctx, snapshotID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("getting snapshot mounts: %v", err),
		})
		return
	}

	// Find a directory where we can read the merged view. For overlayfs,
	// the lowerdir+upperdir together form the container's rootfs. We look
	// in the upperdir first (container-written files), then fall back to
	// searching lowerdirs.
	searchDirs := []string{}
	for _, m := range mounts {
		for _, opt := range m.Options {
			for _, part := range strings.Split(opt, ",") {
				if strings.HasPrefix(part, "upperdir=") {
					searchDirs = append(searchDirs, strings.TrimPrefix(part, "upperdir="))
				}
				if strings.HasPrefix(part, "lowerdir=") {
					searchDirs = append(searchDirs, strings.Split(strings.TrimPrefix(part, "lowerdir="), ":")...)
				}
			}
		}
	}

	var fullPath string
	for _, dir := range searchDirs {
		candidate := filepath.Join(dir, filepath.Clean(srcPath))
		if _, err := os.Stat(candidate); err == nil {
			fullPath = candidate
			break
		}
	}
	if fullPath == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("path %s not found in container", srcPath),
		})
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)

	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()

	base := filepath.Dir(fullPath)
	if err := filepath.Walk(fullPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			if _, cpErr := io.Copy(tw, f); cpErr != nil {
				_ = f.Close()
				return cpErr
			}
			_ = f.Close()
		}
		return nil
	}); err != nil {
		s.log.Debug("creating archive", "error", err)
	}
}

// isCpStdinExec detects the "cp /dev/stdin <path>" pattern used by KIND to
// write config files into containers.
func (s *Server) isCpStdinExec(cmd []string) bool {
	return len(cmd) == 3 && cmd[0] == "cp" && cmd[1] == "/dev/stdin"
}

// handleCpStdinDirect writes stdin data directly to the container's overlayfs
// instead of going through containerd's exec FIFO mechanism. Returns true if
// handled successfully; false if the caller should fall through to normal exec.
func (s *Server) handleCpStdinDirect(conn io.Writer, exec *execEntry, entry *containerEntry, data []byte) bool {
	ctx := namespaces.WithNamespace(context.Background(), s.jobNamespace)

	snapshotID := entry.ID + "-snapshot"
	snapshotter := s.client.SnapshotService("overlayfs")
	if snapshotter == nil {
		return false
	}

	mounts, err := snapshotter.Mounts(ctx, snapshotID)
	if err != nil {
		s.log.Debug("cp-stdin: getting mounts", "exec_id", exec.ID, "error", err)
		return false
	}

	var upperDir string
	for _, m := range mounts {
		for _, opt := range m.Options {
			for _, part := range strings.Split(opt, ",") {
				if strings.HasPrefix(part, "upperdir=") {
					upperDir = strings.TrimPrefix(part, "upperdir=")
				}
			}
		}
	}
	if upperDir == "" {
		return false
	}

	dstPath := exec.Cmd[2]
	hostPath := filepath.Join(upperDir, filepath.Clean(dstPath))

	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		s.log.Warn("cp-stdin: creating parent dir", "exec_id", exec.ID, "error", err)
		return false
	}

	if err := os.WriteFile(hostPath, data, 0o644); err != nil {
		s.log.Warn("cp-stdin: writing file", "exec_id", exec.ID, "path", dstPath, "error", err)
		return false
	}

	exec.ExitCode = 0
	exec.exited = true

	s.log.Info("cp-stdin: wrote directly to overlayfs", "exec_id", exec.ID, "path", dstPath, "bytes", len(data))
	return true
}

// writeStdinFile writes stdin data to a temporary file inside the container's
// overlayfs upperdir. Returns the in-container path on success. The caller
// wraps the original command with shell redirection so the process reads from
// this file instead of relying on containerd's FIFO stdin delivery.
func (s *Server) writeStdinFile(exec *execEntry, entry *containerEntry, data []byte) (string, bool) {
	ctx := namespaces.WithNamespace(context.Background(), s.jobNamespace)

	snapshotID := entry.ID + "-snapshot"
	snapshotter := s.client.SnapshotService("overlayfs")
	if snapshotter == nil {
		return "", false
	}

	mounts, err := snapshotter.Mounts(ctx, snapshotID)
	if err != nil {
		s.log.Debug("stdin-file: getting mounts", "exec_id", exec.ID, "error", err)
		return "", false
	}

	var upperDir string
	for _, m := range mounts {
		for _, opt := range m.Options {
			for _, part := range strings.Split(opt, ",") {
				if strings.HasPrefix(part, "upperdir=") {
					upperDir = strings.TrimPrefix(part, "upperdir=")
				}
			}
		}
	}
	if upperDir == "" {
		return "", false
	}

	containerPath := fmt.Sprintf("/.ephemerd-stdin-%s", exec.ID)
	hostPath := filepath.Join(upperDir, containerPath)
	if err := os.WriteFile(hostPath, data, 0o644); err != nil {
		s.log.Debug("stdin-file: writing file", "exec_id", exec.ID, "error", err)
		return "", false
	}
	return containerPath, true
}

// cleanupExec deletes an exec process and its log.
func (s *Server) cleanupExec(execID string) {
	s.mu.Lock()
	exec, ok := s.execs[execID]
	if ok {
		delete(s.execs, execID)
	}
	s.mu.Unlock()

	if !ok {
		return
	}

	if exec.Process != nil {
		ctx := namespaces.WithNamespace(context.Background(), s.jobNamespace)
		if _, err := exec.Process.Delete(ctx, client.WithProcessKill); err != nil {
			s.log.Debug("exec process delete", "exec_id", execID, "error", err)
		}
	}
	if exec.LogPath != "" {
		_ = os.RemoveAll(filepath.Dir(exec.LogPath))
	}
}

// destroyAllExecs cleans up all exec processes.
func (s *Server) destroyAllExecs() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.execs))
	for id := range s.execs {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	for _, id := range ids {
		s.cleanupExec(id)
	}
}

// --- tar helpers ---

// streamToTempFile writes r to a fresh temp file (using pattern as the
// CreateTemp pattern) and returns the temp file path. The file is always
// closed before returning. On any error the partial temp file is removed.
//
// Extracted so the streaming behavior of copyToViaExec is unit-testable
// without a real containerd. Errors are returned wrapped with context so
// callers can surface them to clients.
func streamToTempFile(r io.Reader, pattern string) (string, error) {
	tmpFile, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, copyErr := io.Copy(tmpFile, r); copyErr != nil {
		// Best-effort cleanup. We surface the copy error to the caller; close
		// and remove failures are joined into the returned error so callers
		// can still see them via errors.Is/As.
		err := fmt.Errorf("writing temp file: %w", copyErr)
		if closeErr := tmpFile.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing temp file: %w", closeErr))
		}
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			err = errors.Join(err, fmt.Errorf("removing temp file: %w", rmErr))
		}
		return "", err
	}
	if closeErr := tmpFile.Close(); closeErr != nil {
		err := fmt.Errorf("closing temp file: %w", closeErr)
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			err = errors.Join(err, fmt.Errorf("removing temp file: %w", rmErr))
		}
		return "", err
	}
	return tmpPath, nil
}

func extractTar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}

		target := filepath.Join(dst, filepath.Clean(hdr.Name))

		// Prevent path traversal.
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) && target != filepath.Clean(dst) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(target) // remove existing before creating symlink
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}
