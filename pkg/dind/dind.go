// Package dind implements a fake Docker Engine API server.
//
// Each job gets its own server instance listening on a Unix socket.
// The socket is bind-mounted into the job container at /var/run/docker.sock.
// Docker CLI calls inside the container are translated into containerd
// operations on the host — no real Docker daemon, no privileged containers.
package dind

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/ephpm/ephemerd/pkg/networking"
)

// sharedNamespace is the containerd namespace used by ephemerd for runner
// containers and cached base images. DinD image pulls check here first
// before pulling into the per-job namespace.
const sharedNamespace = "ephemerd"

// Server is a per-job fake Docker daemon.
type Server struct {
	jobID        string
	jobNamespace string // per-job containerd namespace for isolation
	sockPath     string
	listener     net.Listener
	server       *http.Server
	client       *client.Client
	network      *networking.Manager
	log          *slog.Logger

	mu         sync.Mutex
	images     map[string]*imageEntry    // in-memory image store scoped to this job
	containers map[string]*containerEntry // containers created through this socket
	execs      map[string]*execEntry      // exec processes inside containers
}

type imageEntry struct {
	ID   string `json:"Id"`
	Ref  string `json:"RepoTags"`
	Size int64  `json:"Size"`
}

// Config for creating a per-job fake Docker daemon.
type Config struct {
	// JobID is the unique job identifier.
	JobID string

	// DataDir is the ephemerd data directory. The socket and temp layers
	// are stored under <DataDir>/jobs/<JobID>/docker/.
	DataDir string

	// Client is the containerd client for image pulls and container ops.
	Client *client.Client

	// Network is the networking manager for attaching sibling containers
	// to the CNI bridge. May be nil if networking is not available.
	Network *networking.Manager

	Log *slog.Logger
}

// New creates a fake Docker daemon for a job. Call Start() to begin serving.
func New(cfg Config) (*Server, error) {
	dockerDir := filepath.Join(cfg.DataDir, "jobs", cfg.JobID, "docker")
	if err := os.MkdirAll(dockerDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating docker dir: %w", err)
	}

	// Use a short socket name — Unix sockets have a 108-byte path limit.
	sockPath := filepath.Join(dockerDir, "d.sock")

	// Remove stale socket from a previous crash (best-effort, may not exist)
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		cfg.Log.Debug("removing stale socket", "path", sockPath, "error", err)
	}

	return &Server{
		jobID:        cfg.JobID,
		jobNamespace: "ephemerd/dind/" + cfg.JobID,
		sockPath:     sockPath,
		client:       cfg.Client,
		network:      cfg.Network,
		log:          cfg.Log.With("component", "dind", "job_id", cfg.JobID),
		images:       make(map[string]*imageEntry),
		containers:   make(map[string]*containerEntry),
		execs:        make(map[string]*execEntry),
	}, nil
}

// SocketPath returns the host path to the Unix socket.
// Mount this into the container at /var/run/docker.sock.
func (s *Server) SocketPath() string {
	return s.sockPath
}

// Start begins serving the fake Docker API on the Unix socket.
func (s *Server) Start() error {
	ln, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.sockPath, err)
	}
	s.listener = ln

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.server = &http.Server{Handler: mux}

	go func() {
		if err := s.server.Serve(ln); err != http.ErrServerClosed {
			s.log.Error("fake docker server error", "error", err)
		}
	}()

	s.log.Info("fake docker daemon started", "socket", s.sockPath)
	return nil
}

// Stop shuts down the server and cleans up all per-job state,
// including any containers created through this socket.
func (s *Server) Stop() {
	s.log.Info("stopping fake docker daemon")

	// Destroy all exec processes and containers created through this socket.
	s.destroyAllExecs()
	s.destroyAllContainers()

	if s.server != nil {
		if err := s.server.Shutdown(context.Background()); err != nil {
			s.log.Debug("shutting down fake docker server", "error", err)
		}
	}
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			s.log.Debug("closing listener", "error", err)
		}
	}

	// Clean up the socket and job docker directory
	dockerDir := filepath.Dir(s.sockPath)
	if err := os.RemoveAll(dockerDir); err != nil {
		s.log.Warn("failed to clean docker dir", "path", dockerDir, "error", err)
	}

	s.log.Info("fake docker daemon stopped")
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// Docker CLI sends requests to both /endpoint and /v1.XX/endpoint.
	// We use a router wrapper to strip the version prefix.
	mux.HandleFunc("/", s.route)
}

// route dispatches Docker API requests, stripping the /vN.NN/ prefix if present.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Strip /vN.NN/ prefix (e.g. /v1.45/version → /version)
	if len(path) > 2 && path[1] == 'v' {
		if idx := indexOf(path[2:], '/'); idx >= 0 {
			path = path[2+idx:]
		}
	}

	switch {
	case path == "/_ping":
		s.handlePing(w, r)
	case path == "/version":
		s.handleVersion(w, r)
	case path == "/info":
		s.handleInfo(w, r)
	case path == "/images/json" && r.Method == http.MethodGet:
		s.handleImageList(w, r)
	case path == "/images/create" && r.Method == http.MethodPost:
		s.handleImagePull(w, r)
	case path == "/build" && r.Method == http.MethodPost:
		s.handleImageBuild(w, r)
	case path == "/containers/create" && r.Method == http.MethodPost:
		s.handleContainerCreate(w, r)
	case path == "/containers/json" && r.Method == http.MethodGet:
		s.handleContainerList(w, r)
	case strings.HasPrefix(path, "/containers/"):
		s.routeContainer(w, r, path)
	case strings.HasPrefix(path, "/exec/"):
		s.routeExec(w, r, path)
	default:
		s.handleNotImplemented(w, r)
	}
}

func indexOf(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("API-Version", "1.45")
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("Content-Type", "text/plain")
	if _, err := w.Write([]byte("OK")); err != nil {
		s.log.Debug("writing ping response", "error", err)
	}
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"Version":       "27.0.0-ephemerd",
		"ApiVersion":    "1.45",
		"MinAPIVersion": "1.24",
		"GitCommit":     "ephemerd",
		"GoVersion":     "go1.23",
		"Os":            "linux",
		"Arch":          "amd64",
		"KernelVersion": "",
		"BuildTime":     "",
		"Components": []map[string]any{
			{
				"Name":    "ephemerd",
				"Version": "0.1.0",
				"Details": map[string]string{
					"GitCommit": "ephemerd-dind-shim",
				},
			},
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"ID":                "ephemerd:" + s.jobID,
		"Name":              "ephemerd-dind",
		"ServerVersion":     "27.0.0-ephemerd",
		"OperatingSystem":   "ephemerd (containerd backend)",
		"OSType":            "linux",
		"Architecture":      "x86_64",
		"NCPU":              1,
		"MemTotal":          0,
		"Driver":            "overlayfs",
		"DockerRootDir":     "/var/lib/docker",
		"RegistryConfig":    map[string]any{"InsecureRegistryCIDRs": []string{}, "IndexConfigs": map[string]any{}},
		"SecurityOptions":   []string{},
		"Containers":        s.countContainers(),
		"ContainersRunning": s.countContainersByStatus("running"),
		"ContainersPaused":  0,
		"ContainersStopped": s.countContainersByStatus("exited"),
		"Images":            len(s.images),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleImageList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	images := make([]map[string]any, 0, len(s.images))
	for ref, img := range s.images {
		images = append(images, map[string]any{
			"Id":       img.ID,
			"RepoTags": []string{ref},
			"Size":     img.Size,
		})
	}
	writeJSON(w, http.StatusOK, images)
}

func (s *Server) handleImagePull(w http.ResponseWriter, r *http.Request) {
	fromImage := r.URL.Query().Get("fromImage")
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		tag = "latest"
	}
	ref := fromImage
	if tag != "" && fromImage != "" {
		ref = fromImage + ":" + tag
	}

	if ref == "" {
		http.Error(w, `{"message":"fromImage is required"}`, http.StatusBadRequest)
		return
	}

	if s.client == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "containerd client not available",
		})
		return
	}

	s.log.Info("pulling image", "ref", ref)

	// Stream progress (Docker CLI expects chunked JSON)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeProgress := func(status string) {
		msg := map[string]string{"status": status}
		data, err := json.Marshal(msg)
		if err != nil {
			s.log.Debug("marshaling progress", "error", err)
			return
		}
		if _, err := w.Write(data); err != nil {
			s.log.Debug("writing progress data", "error", err)
			return
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			s.log.Debug("writing progress newline", "error", err)
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeProgress(fmt.Sprintf("Pulling from %s", fromImage))

	// Check the shared ephemerd namespace first — common base images
	// (node:20, ubuntu:24.04, etc.) are cached there across all jobs.
	ctx := r.Context()
	sharedCtx := namespaces.WithNamespace(ctx, sharedNamespace)
	jobCtx := namespaces.WithNamespace(ctx, s.jobNamespace)

	img, err := s.client.GetImage(sharedCtx, ref)
	if err == nil {
		writeProgress("Using cached image from shared namespace")
	} else {
		// Not in shared namespace — pull into the per-job namespace.
		// This keeps private registry images isolated to this job.
		img, err = s.client.Pull(jobCtx, ref, client.WithPullUnpack)
		if err != nil {
			writeProgress(fmt.Sprintf("Error: %v", err))
			return
		}
	}

	size, _ := img.Size(jobCtx)

	s.mu.Lock()
	s.images[ref] = &imageEntry{
		ID:   img.Target().Digest.String(),
		Ref:  ref,
		Size: size,
	}
	s.mu.Unlock()

	writeProgress(fmt.Sprintf("Digest: %s", img.Target().Digest.String()))
	writeProgress(fmt.Sprintf("Status: Downloaded newer image for %s", ref))
}

func (s *Server) handleNotImplemented(w http.ResponseWriter, r *http.Request) {
	s.log.Debug("unimplemented Docker API call", "method", r.Method, "path", r.URL.Path)
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"message": fmt.Sprintf("ephemerd: %s %s is not yet implemented", r.Method, r.URL.Path),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Can't write an error response since we already wrote the status code.
		// Log it for debugging — this typically means the client disconnected.
		slog.Debug("writing JSON response", "error", err)
	}
}
