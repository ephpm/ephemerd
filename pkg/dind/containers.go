package dind

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

const containerNamespace = "ephemerd"

// containerEntry tracks a container created through the fake Docker socket.
type containerEntry struct {
	ID        string
	Name      string
	Image     string
	Cmd       []string
	Env       []string
	Created   time.Time
	Container client.Container
	Task      client.Task
	LogPath   string
	NetNS     string
	IP        string
	Status    string // "created", "running", "exited"
	ExitCode  uint32
}

// createRequest is the subset of Docker's container create body we support.
type createRequest struct {
	Image      string      `json:"Image"`
	Cmd        []string    `json:"Cmd"`
	Entrypoint []string    `json:"Entrypoint"`
	Env        []string    `json:"Env"`
	WorkingDir string      `json:"WorkingDir"`
	HostConfig *hostConfig `json:"HostConfig"`
}

type hostConfig struct {
	Binds       []string `json:"Binds"`
	NetworkMode string   `json:"NetworkMode"`
}

// routeContainer dispatches /containers/{id}/{action} requests.
func (s *Server) routeContainer(w http.ResponseWriter, r *http.Request, path string) {
	rest := strings.TrimPrefix(path, "/containers/")
	parts := strings.SplitN(rest, "/", 2)
	id := s.resolveContainerID(parts[0])

	if len(parts) == 1 {
		if r.Method == http.MethodDelete {
			s.handleContainerRemove(w, r, id)
			return
		}
		s.handleNotImplemented(w, r)
		return
	}

	action := parts[1]
	switch {
	case action == "start" && r.Method == http.MethodPost:
		s.handleContainerStart(w, r, id)
	case action == "stop" && r.Method == http.MethodPost:
		s.handleContainerStop(w, r, id)
	case action == "wait" && r.Method == http.MethodPost:
		s.handleContainerWait(w, r, id)
	case action == "json" && r.Method == http.MethodGet:
		s.handleContainerInspect(w, r, id)
	case action == "logs" && r.Method == http.MethodGet:
		s.handleContainerLogs(w, r, id)
	case action == "exec" && r.Method == http.MethodPost:
		s.handleExecCreate(w, r, id)
	case action == "archive" && r.Method == http.MethodPut:
		s.handleContainerCopyTo(w, r, id)
	case action == "archive" && r.Method == http.MethodGet:
		s.handleContainerCopyFrom(w, r, id)
	default:
		s.handleNotImplemented(w, r)
	}
}

// resolveContainerID resolves a name or short ID to a full container ID.
func (s *Server) resolveContainerID(nameOrID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.containers[nameOrID]; ok {
		return nameOrID
	}
	for id, entry := range s.containers {
		if entry.Name == nameOrID {
			return id
		}
	}
	for id := range s.containers {
		if strings.HasPrefix(id, nameOrID) {
			return id
		}
	}
	return nameOrID
}

func (s *Server) handleContainerCreate(w http.ResponseWriter, r *http.Request) {
	if s.client == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "containerd client not available",
		})
		return
	}

	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": fmt.Sprintf("invalid request body: %v", err),
		})
		return
	}

	if req.Image == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "Image is required",
		})
		return
	}

	id := generateContainerID()
	name := strings.TrimPrefix(r.URL.Query().Get("name"), "/")

	ctx := namespaces.WithNamespace(r.Context(), containerNamespace)

	// Resolve image from containerd, pulling if needed.
	img, err := s.client.GetImage(ctx, req.Image)
	if err != nil {
		s.log.Info("image not found, pulling for container create", "image", req.Image)
		img, err = s.client.Pull(ctx, req.Image, client.WithPullUnpack)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"message": fmt.Sprintf("image %s not found: %v", req.Image, err),
			})
			return
		}
	}

	// Build OCI spec. Always target Linux — dind containers are Linux.
	targetPlatform := "linux/" + goruntime.GOARCH
	opts := []oci.SpecOpts{
		oci.WithDefaultSpecForPlatform(targetPlatform),
		oci.WithImageConfig(img),
	}

	if len(req.Cmd) > 0 {
		opts = append(opts, oci.WithProcessArgs(req.Cmd...))
	} else if len(req.Entrypoint) > 0 {
		opts = append(opts, oci.WithProcessArgs(req.Entrypoint...))
	}

	if len(req.Env) > 0 {
		opts = append(opts, oci.WithEnv(req.Env))
	}

	if req.WorkingDir != "" {
		opts = append(opts, oci.WithProcessCwd(req.WorkingDir))
	}

	// Bind mounts from HostConfig.
	if req.HostConfig != nil {
		for _, bind := range req.HostConfig.Binds {
			bindParts := strings.SplitN(bind, ":", 3)
			if len(bindParts) >= 2 {
				mountOpts := []string{"rbind", "rw"}
				if len(bindParts) == 3 && bindParts[2] == "ro" {
					mountOpts = []string{"rbind", "ro"}
				}
				opts = append(opts, withBindMount(bindParts[0], bindParts[1], mountOpts))
			}
		}
	}

	snapshotName := id + "-snapshot"
	container, err := s.client.NewContainer(ctx, id,
		client.WithImage(img),
		client.WithSnapshotter("overlayfs"),
		client.WithNewSnapshot(snapshotName, img),
		client.WithNewSpec(opts...),
		client.WithRuntime("io.containerd.runc.v2", nil),
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("creating container: %v", err),
		})
		return
	}

	entry := &containerEntry{
		ID:        id,
		Name:      name,
		Image:     req.Image,
		Cmd:       req.Cmd,
		Env:       req.Env,
		Created:   time.Now(),
		Container: container,
		Status:    "created",
	}

	s.mu.Lock()
	s.containers[id] = entry
	s.mu.Unlock()

	s.log.Info("container created", "id", id, "name", name, "image", req.Image)

	writeJSON(w, http.StatusCreated, map[string]any{
		"Id":       id,
		"Warnings": []string{},
	})
}

func (s *Server) handleContainerStart(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	entry, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", id),
		})
		return
	}

	if entry.Status == "running" {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	ctx := namespaces.WithNamespace(r.Context(), containerNamespace)

	// Create log directory for capturing stdout/stderr.
	logDir := filepath.Join(filepath.Dir(s.sockPath), "containers", id)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("creating log dir: %v", err),
		})
		return
	}
	logPath := filepath.Join(logDir, "output.log")
	entry.LogPath = logPath

	task, err := entry.Container.NewTask(ctx, cio.LogFile(logPath))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("creating task: %v", err),
		})
		return
	}
	entry.Task = task

	// Attach CNI networking before starting the task.
	if s.network != nil {
		pid := task.Pid()
		netns := fmt.Sprintf("/proc/%d/ns/net", pid)
		result, err := s.network.Setup(ctx, id, netns)
		if err != nil {
			s.log.Warn("failed to setup network for dind container", "id", id, "error", err)
		} else {
			entry.NetNS = result.NetNS
			entry.IP = result.IP
			s.log.Info("network attached to dind container", "id", id, "ip", entry.IP)
		}
	}

	if err := task.Start(ctx); err != nil {
		// Clean up on failure.
		if _, delErr := task.Delete(ctx, client.WithProcessKill); delErr != nil {
			s.log.Debug("task cleanup after failed start", "error", delErr)
		}
		if s.network != nil && entry.NetNS != "" {
			if tearErr := s.network.Teardown(ctx, id, entry.NetNS); tearErr != nil {
				s.log.Debug("network cleanup after failed start", "error", tearErr)
			}
			entry.NetNS = ""
			entry.IP = ""
		}
		entry.Task = nil
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("starting task: %v", err),
		})
		return
	}

	entry.Status = "running"
	s.log.Info("container started", "id", id, "ip", entry.IP)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleContainerInspect(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	entry, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", id),
		})
		return
	}

	// Refresh status from containerd.
	status := entry.Status
	exitCode := entry.ExitCode
	if entry.Task != nil {
		ctx := namespaces.WithNamespace(r.Context(), containerNamespace)
		if taskStatus, err := entry.Task.Status(ctx); err == nil {
			switch taskStatus.Status {
			case client.Running:
				status = "running"
			case client.Stopped:
				status = "exited"
				exitCode = taskStatus.ExitStatus
			}
		}
	}

	displayName := entry.Name
	if displayName == "" {
		displayName = id[:12]
	}

	resp := map[string]any{
		"Id":    entry.ID,
		"Name":  "/" + displayName,
		"Image": entry.Image,
		"State": map[string]any{
			"Status":   status,
			"Running":  status == "running",
			"ExitCode": exitCode,
		},
		"Config": map[string]any{
			"Image": entry.Image,
			"Cmd":   entry.Cmd,
			"Env":   entry.Env,
		},
		"NetworkSettings": map[string]any{
			"IPAddress": entry.IP,
			"Networks": map[string]any{
				"bridge": map[string]any{
					"IPAddress": entry.IP,
				},
			},
		},
		"Created": entry.Created.Format(time.RFC3339Nano),
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleContainerStop(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	entry, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", id),
		})
		return
	}

	if entry.Task == nil || entry.Status != "running" {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	ctx := namespaces.WithNamespace(r.Context(), containerNamespace)

	if err := entry.Task.Kill(ctx, 15); err != nil {
		s.log.Debug("SIGTERM failed, sending SIGKILL", "id", id, "error", err)
		if killErr := entry.Task.Kill(ctx, 9); killErr != nil {
			s.log.Debug("SIGKILL also failed", "id", id, "error", killErr)
		}
	}

	exitCh, err := entry.Task.Wait(ctx)
	if err == nil {
		select {
		case status := <-exitCh:
			entry.ExitCode = status.ExitCode()
		case <-time.After(10 * time.Second):
			if killErr := entry.Task.Kill(ctx, 9); killErr != nil {
				s.log.Debug("timeout SIGKILL failed", "id", id, "error", killErr)
			}
		}
	}

	entry.Status = "exited"
	s.log.Info("container stopped", "id", id)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleContainerWait(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	entry, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", id),
		})
		return
	}

	if entry.Task == nil {
		writeJSON(w, http.StatusOK, map[string]any{"StatusCode": 0})
		return
	}

	// If already exited, return immediately.
	if entry.Status == "exited" {
		writeJSON(w, http.StatusOK, map[string]any{"StatusCode": entry.ExitCode})
		return
	}

	ctx := namespaces.WithNamespace(r.Context(), containerNamespace)
	exitCh, err := entry.Task.Wait(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("waiting for task: %v", err),
		})
		return
	}

	select {
	case status := <-exitCh:
		entry.ExitCode = status.ExitCode()
		entry.Status = "exited"
		writeJSON(w, http.StatusOK, map[string]any{"StatusCode": entry.ExitCode})
	case <-r.Context().Done():
		writeJSON(w, http.StatusRequestTimeout, map[string]string{
			"message": "request cancelled",
		})
	}
}

func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	entry, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", id),
		})
		return
	}

	if entry.LogPath == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	data, err := os.ReadFile(entry.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": fmt.Sprintf("reading logs: %v", err),
		})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		s.log.Debug("writing log response", "error", err)
	}
}

func (s *Server) handleContainerRemove(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	entry, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", id),
		})
		return
	}

	ctx := namespaces.WithNamespace(r.Context(), containerNamespace)
	s.cleanupContainer(ctx, id, entry)

	s.mu.Lock()
	delete(s.containers, id)
	s.mu.Unlock()

	s.log.Info("container removed", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleContainerList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]map[string]any, 0, len(s.containers))
	for _, entry := range s.containers {
		names := []string{}
		if entry.Name != "" {
			names = []string{"/" + entry.Name}
		}
		result = append(result, map[string]any{
			"Id":      entry.ID,
			"Names":   names,
			"Image":   entry.Image,
			"State":   entry.Status,
			"Created": entry.Created.Unix(),
			"NetworkSettings": map[string]any{
				"Networks": map[string]any{
					"bridge": map[string]any{
						"IPAddress": entry.IP,
					},
				},
			},
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// cleanupContainer kills, deletes, and tears down networking for a container.
func (s *Server) cleanupContainer(ctx context.Context, id string, entry *containerEntry) {
	if entry.Task != nil {
		taskStatus, err := entry.Task.Status(ctx)
		if err == nil && taskStatus.Status == client.Running {
			if killErr := entry.Task.Kill(ctx, 9); killErr != nil {
				s.log.Debug("kill during cleanup", "id", id, "error", killErr)
			}
			exitCh, err := entry.Task.Wait(ctx)
			if err == nil {
				<-exitCh
			}
		}
		if _, err := entry.Task.Delete(ctx, client.WithProcessKill); err != nil {
			s.log.Debug("task delete during cleanup", "id", id, "error", err)
		}
	}

	if s.network != nil && entry.NetNS != "" {
		if err := s.network.Teardown(ctx, id, entry.NetNS); err != nil {
			s.log.Debug("network teardown during cleanup", "id", id, "error", err)
		}
	}

	if entry.Container != nil {
		if err := entry.Container.Delete(ctx, client.WithSnapshotCleanup); err != nil {
			s.log.Debug("container delete during cleanup", "id", id, "error", err)
		}
	}

	if entry.LogPath != "" {
		if err := os.RemoveAll(filepath.Dir(entry.LogPath)); err != nil {
			s.log.Debug("log cleanup", "id", id, "error", err)
		}
	}
}

// destroyAllContainers cleans up every container in the map.
func (s *Server) destroyAllContainers() {
	ctx := namespaces.WithNamespace(context.Background(), containerNamespace)

	s.mu.Lock()
	snapshot := make(map[string]*containerEntry, len(s.containers))
	for k, v := range s.containers {
		snapshot[k] = v
	}
	s.containers = make(map[string]*containerEntry)
	s.mu.Unlock()

	for id, entry := range snapshot {
		s.log.Info("destroying dind container on shutdown", "id", id)
		s.cleanupContainer(ctx, id, entry)
	}
}

func (s *Server) countContainers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.containers)
}

func (s *Server) countContainersByStatus(status string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, entry := range s.containers {
		if entry.Status == status {
			n++
		}
	}
	return n
}

func generateContainerID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Fallback — this should never fail.
		return fmt.Sprintf("dind-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func withBindMount(src, dst string, options []string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *ocispec.Spec) error {
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: dst,
			Type:        "bind",
			Source:      src,
			Options:     options,
		})
		return nil
	}
}
