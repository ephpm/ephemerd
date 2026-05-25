// Package dind implements a fake Docker Engine API server.
//
// Each job gets its own server instance listening on a Unix socket.
// The socket is bind-mounted into the job container at /var/run/docker.sock.
// Docker CLI calls inside the container are translated into containerd
// operations on the host — no real Docker daemon.
//
// Sibling containers created through this API can opt into the full
// docker --privileged elevation stack (all caps, all devices, seccomp
// and apparmor unconfined, writable sysfs/cgroupfs) when the host is
// configured to allow it. See Server.allowPrivileged and
// config.DindConfig.AllowPrivileged for the threat model — the short
// version is that on Windows and macOS hosts the dind containerd lives
// inside a managed Linux VM, so an escape stays in that VM; on a Linux
// host with no VM fence the default is to deny privileged requests.
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
	goruntime "runtime"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/ephpm/ephemerd/pkg/buildkit"
	"github.com/ephpm/ephemerd/pkg/networking"
)

// sharedNamespace is the containerd namespace used by ephemerd for runner
// containers and cached base images. DinD image pulls check here first
// before pulling into the per-job namespace.
const sharedNamespace = "ephemerd"

// Server is a per-job fake Docker daemon.
type Server struct {
	jobID           string
	jobNamespace    string // per-job containerd namespace for isolation
	cacheNamespace  string // per-(provider,repo) shared image cache namespace; empty disables caching
	sockPath        string // host-side unix socket path (Linux/macOS only)
	endpoint        string // what the container should set DOCKER_HOST to (e.g. "tcp://gw:port" on Windows)
	listener        net.Listener
	server          *http.Server
	client          *client.Client
	network         *networking.Manager
	buildkit        *buildkit.Server // shared embedded BuildKit solver (nil → fall back to platform default)
	runnerNetNS     string           // path to runner container's net namespace; used to install DNAT rules for port bindings
	allowPrivileged bool             // gate for docker run --privileged / --cap-add; see config.DindConfig.AllowPrivileged
	log             *slog.Logger

	mu         sync.Mutex
	images     map[string]*imageEntry     // in-memory image store scoped to this job
	containers map[string]*containerEntry // containers created through this socket
	execs      map[string]*execEntry      // exec processes inside containers
	networks   map[string]*networkEntry   // Docker networks (logical, in-memory)
	auth       authCache                  // per-job docker login cache (registry host → creds)

	// rootfsSearchDirsFn resolves the host-filesystem directories that
	// together form the merged rootfs view for a container snapshot.
	// Nil in production — handleContainerStatPath / handleContainerCopyFrom
	// fall through to the real containerd snapshotter path. Tests stub
	// this to avoid standing up containerd.
	rootfsSearchDirsFn func(ctx context.Context, snapshotID string) ([]string, error)
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

	// Provider is the forge provider name ("github", "gitea", "forgejo",
	// "gitlab", "woodpecker") for the job. Used together with Repo to
	// build the per-repo image cache namespace; if empty, image caching
	// across jobs is disabled and every pull is cold for this job.
	Provider string

	// Repo is the forge-native repo path (e.g. "owner/repo" on GitHub
	// or "group/subgroup/project" on GitLab). Used together with Provider
	// to build the per-repo image cache namespace; if empty, image
	// caching across jobs is disabled.
	Repo string

	// DataDir is the ephemerd data directory. The socket and temp layers
	// are stored under <DataDir>/jobs/<JobID>/docker/.
	DataDir string

	// Client is the containerd client for image pulls and container ops.
	Client *client.Client

	// Network is the networking manager for attaching sibling containers
	// to the CNI bridge. May be nil if networking is not available.
	Network *networking.Manager

	// BuildKit is the shared embedded BuildKit solver. When non-nil,
	// POST /build routes through handleImageBuildBuildkit. When nil, the
	// platform default (buildah on Linux, 501 elsewhere) is used.
	BuildKit *buildkit.Server

	// RunnerNetNS is the path to the runner container's net namespace
	// (e.g. /proc/<pid>/ns/net). Required for port forwarding — when a
	// dind sibling exposes ports via -p, we install iptables DNAT rules
	// in this namespace so the runner sees 127.0.0.1:hostPort routed to
	// the sibling's container IP. Empty disables port forwarding.
	RunnerNetNS string

	// AllowPrivileged controls whether sibling containers may opt into
	// the full elevation stack via HostConfig.Privileged or via
	// HostConfig.CapAdd. When false, requests carrying either are
	// rejected with HTTP 403. See config.DindConfig.AllowPrivileged for
	// the threat model.
	AllowPrivileged bool

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

	s := &Server{
		jobID: cfg.JobID,
		// containerd namespace name regex (^[A-Za-z0-9]+(?:[._-](?:[A-Za-z0-9]+))*$)
		// rejects slashes. Use hyphens to namespace per-job dind state.
		jobNamespace:    "ephemerd-dind-" + cfg.JobID,
		cacheNamespace:  CacheNamespace(cfg.Provider, cfg.Repo),
		sockPath:        sockPath,
		client:          cfg.Client,
		network:         cfg.Network,
		buildkit:        cfg.BuildKit,
		runnerNetNS:     cfg.RunnerNetNS,
		allowPrivileged: cfg.AllowPrivileged,
		log:             cfg.Log.With("component", "dind", "job_id", cfg.JobID),
		images:          make(map[string]*imageEntry),
		containers:      make(map[string]*containerEntry),
		execs:           make(map[string]*execEntry),
		networks:        make(map[string]*networkEntry),
	}
	s.initDefaultBridgeNetwork()
	return s, nil
}

// SocketPath returns the host-side Unix socket path (Linux/macOS).
// Empty on Windows — use Endpoint() to get the DOCKER_HOST value instead.
func (s *Server) SocketPath() string {
	return s.sockPath
}

// SetRunnerNetNS records the runner container's net namespace path so the
// dind server can install iptables DNAT rules for port-bound siblings. Must
// be called after the runner task starts (PID is known) and before any
// docker create from inside the runner.
func (s *Server) SetRunnerNetNS(netnsPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runnerNetNS = netnsPath
}

// Endpoint returns the value a container should set DOCKER_HOST to in order
// to reach this fake daemon. Linux/macOS return the unix socket path directly
// (e.g. "unix:///var/run/docker.sock" once bind-mounted); Windows returns a
// "tcp://<gateway-ip>:<port>" URI pointing at a TCP listener bound on the
// HCN NAT gateway so containers on the NAT can reach it without any mount.
func (s *Server) Endpoint() string {
	return s.endpoint
}

// Start begins serving the fake Docker API.
//
// Linux/macOS: listens on a per-job unix socket at <DataDir>/jobs/<jobID>/docker/d.sock.
// Windows: listens on TCP on the HCN NAT gateway IP (picked from networking.Manager)
// so Hyper-V-isolated runner containers on the same NAT can reach it without a
// runhcs bind mount (which isn't supported) or named pipe sharing (which needs
// HCS config for isolated containers).
func (s *Server) Start() error {
	ln, err := s.listen()
	if err != nil {
		return err
	}
	// Make socket world-accessible so non-root container processes can connect.
	if err := os.Chmod(s.sockPath, 0o666); err != nil {
		s.log.Warn("failed to chmod docker socket", "error", err)
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

	s.log.Info("fake docker daemon started", "endpoint", s.endpoint)
	return nil
}

// Stop shuts down the server and cleans up all per-job state,
// including any containers created through this socket.
func (s *Server) Stop() {
	s.log.Info("stopping fake docker daemon")

	// Destroy all exec processes and containers created through this socket.
	s.destroyAllExecs()
	s.destroyAllContainers()

	// Clean up the per-job containerd namespace. destroyAllContainers handles
	// containers tracked in the in-memory map; this catches stragglers
	// (kindest/node-side containerd creations that landed in the same
	// namespace but never registered in s.containers), then deletes the
	// Image and Lease records so containerd's content GC can reclaim the
	// pinned blobs, then drops the namespace metadata bucket itself.
	// Without this, every job leaks ~1 GB of image content + the snapshot
	// upperdir referenced by un-deleted Image records.
	if s.client != nil {
		CleanupJobNamespace(context.Background(), s.client, s.jobNamespace, s.log)
	}

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
		// /build always goes through the embedded BuildKit solver. If it
		// isn't configured (e.g. dind disabled, or worker-mode init failure),
		// 501 — there is no buildah fallback. The router refuses to silently
		// pick a different builder and risk a mismatch with the namespace
		// /push reads from.
		if s.buildkit == nil {
			writeJSON(w, http.StatusNotImplemented, map[string]string{
				"message": "docker build requires BuildKit; ephemerd was not started with --dind or BuildKit init failed",
			})
			return
		}
		s.handleImageBuildBuildkit(s.buildkit)(w, r)
	case path == "/auth" && r.Method == http.MethodPost:
		s.handleAuth(w, r)
	case strings.HasSuffix(path, "/push") && strings.HasPrefix(path, "/images/") && r.Method == http.MethodPost:
		// Strip "/images/" prefix and "/push" suffix to get the URL-encoded ref.
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/push")
		s.handleImagePush(w, r, ref)
	case path == "/containers/create" && r.Method == http.MethodPost:
		s.handleContainerCreate(w, r)
	case path == "/containers/json" && r.Method == http.MethodGet:
		s.handleContainerList(w, r)
	case strings.HasPrefix(path, "/containers/"):
		s.routeContainer(w, r, path)
	case strings.HasPrefix(path, "/exec/"):
		s.routeExec(w, r, path)
	case (path == "/networks" || path == "/networks/json") && r.Method == http.MethodGet:
		s.handleNetworkList(w, r)
	case path == "/networks/create" && r.Method == http.MethodPost:
		s.handleNetworkCreate(w, r)
	case path == "/networks/prune" && r.Method == http.MethodPost:
		s.handleNetworkPrune(w, r)
	case strings.HasPrefix(path, "/networks/"):
		s.routeNetwork(w, r, path)
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
	// Docker CLI's `docker build` cross-checks the daemon's reported "Os"
	// against the client OS; a mismatch fires the
	// "building a Docker image from Windows against a non-Windows Docker
	// host" security warning. Report the host OS we actually run on.
	osName := goruntime.GOOS
	resp := map[string]any{
		"Version":       "27.0.0-ephemerd",
		"ApiVersion":    "1.45",
		"MinAPIVersion": "1.24",
		"GitCommit":     "ephemerd",
		"GoVersion":     "go1.23",
		"Os":            osName,
		"Arch":          goruntime.GOARCH,
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
	// Tell the docker CLI we speak BuildKit. Without this, the CLI's
	// daemon-feature probe falls back to the legacy builder and buildx
	// defaults to its docker-container driver (which tries to pull
	// moby/buildkit:* — no Windows variant exists, fails to resolve).
	// "BuilderVersion": "2" is BuildKit; "1" is legacy.
	osType := "linux"
	driver := "overlayfs"
	if goruntime.GOOS == "windows" {
		osType = "windows"
		driver = "windowsfilter"
	}
	resp := map[string]any{
		"ID":                "ephemerd:" + s.jobID,
		"Name":              "ephemerd-dind",
		"ServerVersion":     "27.0.0-ephemerd",
		"OperatingSystem":   "ephemerd (containerd backend)",
		"OSType":            osType,
		"Architecture":      goruntime.GOARCH,
		"NCPU":              1,
		"MemTotal":          0,
		"Driver":            driver,
		"DockerRootDir":     "/var/lib/docker",
		"BuilderVersion":    "2",
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
	unqualifiedRef := fromImage
	if tag != "" && fromImage != "" {
		unqualifiedRef = fromImage + ":" + tag
	}

	if unqualifiedRef == "" {
		http.Error(w, `{"message":"fromImage is required"}`, http.StatusBadRequest)
		return
	}

	// containerd's resolver treats the first path segment of an unqualified
	// reference as the registry hostname, so "moby/buildkit:buildx-stable-1"
	// (sent by docker buildx setup) tries to dial host "moby". Force the
	// docker.io qualifier for the network pull, but ALSO register the image
	// under the unqualified name so subsequent docker inspect / docker run
	// calls (which use the original Docker-CLI form) find it without
	// triggering a re-pull.
	ref := qualifyDockerHubRef(unqualifiedRef)

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

	// Register the image under both the qualified pull ref and the
	// original unqualified ref so subsequent docker inspect / run /
	// containerd lookups (which carry whatever name the CLI sent) find
	// it without a re-pull.
	if ref != unqualifiedRef {
		imgSvc := s.client.ImageService()
		if _, err := imgSvc.Create(jobCtx, images.Image{
			Name:   unqualifiedRef,
			Target: img.Target(),
			Labels: map[string]string{"ephemerd.alias-of": ref},
		}); err != nil {
			// Update if Create says already-exists — happens on second pull.
			if _, uerr := imgSvc.Update(jobCtx, images.Image{
				Name:   unqualifiedRef,
				Target: img.Target(),
				Labels: map[string]string{"ephemerd.alias-of": ref},
			}); uerr != nil {
				s.log.Warn("aliasing image under unqualified name", "ref", unqualifiedRef, "error", uerr)
			}
		}
	}

	s.mu.Lock()
	s.images[unqualifiedRef] = &imageEntry{
		ID:   img.Target().Digest.String(),
		Ref:  unqualifiedRef,
		Size: size,
	}
	s.mu.Unlock()

	// Mirror the Image record into the per-repo cache namespace so the
	// gc.ref labels keep the manifest+config+layer blobs alive after the
	// per-job namespace is cleaned up. Next job in the same repo gets a
	// content-store hit. Cross-repo / cross-provider jobs do NOT see this
	// cache record (namespace isolation), so private images don't leak.
	if s.cacheNamespace != "" {
		// Mirror both the qualified ref (what containerd pulled under)
		// and the unqualified docker-CLI form, so future cache hits via
		// either name work.
		for _, name := range dedup(ref, unqualifiedRef) {
			if err := MirrorImageToCache(ctx, s.client, s.jobNamespace, s.cacheNamespace, name, s.log); err != nil {
				s.log.Debug("dind cache: mirror failed", "image", name, "error", err)
			}
		}
	}

	writeProgress(fmt.Sprintf("Digest: %s", img.Target().Digest.String()))
	writeProgress(fmt.Sprintf("Status: Downloaded newer image for %s", unqualifiedRef))
}

// dedup returns the unique non-empty strings from the input in the order
// they first appear. Used so we don't mirror the same image twice when
// qualified and unqualified forms match.
func dedup(in ...string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
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
