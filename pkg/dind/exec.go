package dind

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// execEntry tracks an exec process created inside a running container.
// Task.Exec is called lazily at /exec/start so stdio can be wired to the
// hijacked TCP connection (not available at /exec/create time).
type execEntry struct {
	ID          string
	ContainerID string
	Cmd         []string
	Env         []string
	WorkingDir  string
	Tty         bool

	// pspec is the prepared OCI process spec used when Task.Exec is called.
	pspec *ocispec.Process
	// task is the parent container task we'll exec against. Kept on the
	// entry so a later /exec/start doesn't have to re-resolve the container.
	task client.Task

	// Process is nil until the first /exec/start successfully invokes Task.Exec.
	Process  client.Process
	LogPath  string // only set on the non-hijacked (buffered) path
	Running  bool
	ExitCode int
	exited   bool
}

// execCreateRequest is the Docker exec create request body.
type execCreateRequest struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env"`
	WorkingDir   string   `json:"WorkingDir"`
}

// execStartRequest is the Docker exec start request body. All fields are
// optional; clients often send {} and rely on create-time defaults.
type execStartRequest struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
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

	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)

	// Build OCI process spec.
	pspec := &ocispec.Process{
		Args: req.Cmd,
		Cwd:  "/",
		User: ocispec.User{UID: 0, GID: 0},
	}
	if req.WorkingDir != "" {
		pspec.Cwd = req.WorkingDir
	}

	// Inherit container env, then overlay exec-specific env.
	spec, err := entry.Container.Spec(ctx)
	if err == nil && spec.Process != nil {
		pspec.Env = spec.Process.Env
	}
	pspec.Env = append(pspec.Env, req.Env...)

	// Don't call Task.Exec yet — stdio is wired at /exec/start once we
	// know whether the caller is hijacking for streamed IO or taking the
	// detached/buffered path.
	exec := &execEntry{
		ID:          execID,
		ContainerID: containerID,
		Cmd:         req.Cmd,
		Env:         req.Env,
		WorkingDir:  pspec.Cwd,
		Tty:         req.Tty,
		pspec:       pspec,
		task:        entry.Task,
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
	if !ok {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("exec %s not found", execID),
		})
		return
	}
	if exec.Running || exec.exited {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	// Claim the slot before releasing the lock so a parallel /exec/start for
	// the same execID loses the race cleanly (second one sees Running=true).
	exec.Running = true
	s.mu.Unlock()

	// Parse Detach/Tty from body; body is optional and often empty "{}".
	var startReq execStartRequest
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&startReq)
	}
	tty := exec.Tty || startReq.Tty

	if wantsHijack(r) {
		s.runHijackedExec(w, r, exec, tty)
		return
	}
	// No upgrade headers: pick behavior by Detach flag.
	s.runBufferedExec(w, r, exec, startReq.Detach)
}

// runHijackedExec wires the containerd exec process's stdio to a hijacked
// TCP connection so real Docker SDK clients (buildx, docker CLI) see the
// expected 101 + raw/multiplexed stream protocol.
func (s *Server) runHijackedExec(w http.ResponseWriter, r *http.Request, exec *execEntry, tty bool) {
	contentType := contentTypeMuxStream
	if tty {
		contentType = contentTypeRawStream
	}

	// Take over the connection BEFORE calling Task.Exec — if hijack fails
	// we roll back cleanly without leaving an exec process dangling.
	conn, buf, err := hijackConn(w, contentType)
	if err != nil {
		s.mu.Lock()
		exec.Running = false
		s.mu.Unlock()
		s.log.Error("exec hijack failed", "exec_id", exec.ID, "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	ctx := namespaces.WithNamespace(context.Background(), s.jobNamespace)

	// stdin: raw bytes from the hijacked conn (client keeps appending to
	// the same TCP stream after the 101). Any bytes the HTTP server already
	// buffered are exposed via buf — read from there first.
	stdinR, stdinW := io.Pipe()
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		// The bufio.Reader includes any pipelined pre-hijack bytes; its
		// underlying reader is the hijacked conn so EOF here = client closed.
		if _, copyErr := io.Copy(stdinW, buf); copyErr != nil && copyErr != io.EOF {
			s.log.Debug("exec stdin copy ended", "exec_id", exec.ID, "error", copyErr)
		}
		// Closing stdinW makes the process see EOF on its stdin.
		_ = stdinW.Close()
	}()

	// stdout/stderr: framed onto the hijacked conn. In TTY mode there's a
	// single stream (stdout only — Docker attaches both to the PTY there).
	var stdout, stderr io.Writer
	if tty {
		raw := &rawStreamWriter{w: conn}
		stdout = raw
		stderr = nil
	} else {
		mux := newStreamMux(conn)
		stdout = &streamMuxWriter{mux: mux, stream: stdcopyStdout}
		stderr = &streamMuxWriter{mux: mux, stream: stdcopyStderr}
	}

	ioCreator := cio.NewCreator(cio.WithStreams(stdinR, stdout, stderr))
	proc, err := exec.task.Exec(ctx, exec.ID, exec.pspec, ioCreator)
	if err != nil {
		s.log.Error("exec task.Exec failed", "exec_id", exec.ID, "error", err)
		s.markExecExited(exec, 1)
		return
	}
	s.mu.Lock()
	exec.Process = proc
	s.mu.Unlock()

	statusCh, err := proc.Wait(ctx)
	if err != nil {
		s.log.Error("exec wait registration failed", "exec_id", exec.ID, "error", err)
		s.markExecExited(exec, 1)
		return
	}
	if err := proc.Start(ctx); err != nil {
		s.log.Error("exec start failed", "exec_id", exec.ID, "error", err)
		s.markExecExited(exec, 1)
		return
	}
	s.log.Info("exec started (hijacked)", "exec_id", exec.ID, "tty", tty)

	// Wait for process exit or client cancellation. The conn is ours now;
	// closing it on return unblocks any pending writes and signals EOF to
	// the client.
	select {
	case status := <-statusCh:
		code := int(status.ExitCode())
		s.markExecExited(exec, code)
		s.log.Info("exec exited (hijacked)", "exec_id", exec.ID, "exit_code", code)
	case <-r.Context().Done():
		// HTTP request context cancelled — best-effort kill.
		if _, err := proc.Delete(ctx, client.WithProcessKill); err != nil {
			s.log.Debug("exec kill after cancel", "exec_id", exec.ID, "error", err)
		}
		s.markExecExited(exec, 137)
	}
	// Don't wait for stdinDone here — once the process has exited, any further
	// stdin bytes from the client are meaningless, and the goroutine is
	// blocked on conn.Read (the client typically holds the conn open until it
	// sees EOF). The deferred conn.Close() below unblocks that read, letting
	// the goroutine exit on its own; Go will clean it up. Waiting on stdinDone
	// before the defer ran caused a server↔client deadlock where the client
	// waited for EOF and we waited for stdin to finish.
	_ = stdinDone
}

// runBufferedExec is the legacy non-streaming path: start the exec with
// output going to a log file, block until it exits, and return the captured
// output as the response body. Fine for simple HTTP probe use cases but NOT
// compatible with Docker SDK clients that hijack. Kept for code paths that
// deliberately avoid hijack (e.g., JSON-only health probes) and when Detach
// is set (fire-and-forget).
func (s *Server) runBufferedExec(w http.ResponseWriter, r *http.Request, exec *execEntry, detach bool) {
	ctx := namespaces.WithNamespace(context.Background(), s.jobNamespace)

	logDir := filepath.Join(filepath.Dir(s.sockPath), "exec", exec.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		s.mu.Lock()
		exec.Running = false
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("creating exec log dir: %v", err),
		})
		return
	}
	logPath := filepath.Join(logDir, "output.log")
	exec.LogPath = logPath

	proc, err := exec.task.Exec(ctx, exec.ID, exec.pspec, cio.LogFile(logPath))
	if err != nil {
		s.mu.Lock()
		exec.Running = false
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("creating exec: %v", err),
		})
		return
	}
	s.mu.Lock()
	exec.Process = proc
	s.mu.Unlock()

	statusCh, err := proc.Wait(ctx)
	if err != nil {
		s.markExecExited(exec, 1)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("waiting for exec: %v", err),
		})
		return
	}
	if err := proc.Start(ctx); err != nil {
		s.markExecExited(exec, 1)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("starting exec: %v", err),
		})
		return
	}

	if detach {
		// Fire-and-forget: return immediately, let process run in background.
		// A goroutine collects the final status so inspect sees the exit code.
		go func() {
			status := <-statusCh
			s.markExecExited(exec, int(status.ExitCode()))
		}()
		w.WriteHeader(http.StatusOK)
		return
	}

	select {
	case status := <-statusCh:
		s.markExecExited(exec, int(status.ExitCode()))
	case <-r.Context().Done():
		s.markExecExited(exec, 137)
		writeJSON(w, http.StatusRequestTimeout, map[string]string{
			"message": "exec timed out",
		})
		return
	}

	w.Header().Set("Content-Type", contentTypeRawStream)
	w.WriteHeader(http.StatusOK)
	if data, err := os.ReadFile(logPath); err == nil {
		if _, writeErr := w.Write(data); writeErr != nil {
			s.log.Debug("writing exec output", "error", writeErr)
		}
	}
}

// markExecExited updates exec state atomically after the process finishes.
func (s *Server) markExecExited(exec *execEntry, code int) {
	s.mu.Lock()
	exec.Running = false
	exec.exited = true
	exec.ExitCode = code
	s.mu.Unlock()
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
	tmpFile, err := os.CreateTemp("", "dind-copy-*.tar")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("creating temp file: %v", err),
		})
		return
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := io.Copy(tmpFile, r.Body); err != nil {
		_ = tmpFile.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("writing temp tar: %v", err),
		})
		return
	}
	_ = tmpFile.Close()

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
