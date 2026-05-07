package dind

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
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

// fileClosingIO wraps a cio.IO to also close the log file on cleanup.
type fileClosingIO struct {
	cio.IO
	file *os.File
}

func (f *fileClosingIO) Close() error {
	ioErr := f.IO.Close()
	fileErr := f.file.Close()
	if ioErr != nil {
		return ioErr
	}
	return fileErr
}

// logFileTerminal creates a cio.Creator with Terminal=true. The containerd
// shim allocates a real PTY: the slave becomes the container's /dev/console
// and stdio, the master output is copied to FIFO-based streams that we drain
// into the log file. systemd sees a real terminal (isatty=true) and prints
// status messages like "Reached target Multi-User System" that KIND needs.
func logFileTerminal(path string) cio.Creator {
	return func(id string) (cio.IO, error) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("opening log file for terminal IO: %w", err)
		}
		creator := cio.NewCreator(
			cio.WithTerminal,
			cio.WithStreams(nil, f, f),
		)
		io, err := creator(id)
		if err != nil {
			if closeErr := f.Close(); closeErr != nil {
				slog.Warn("failed to close log file after creator error", "error", closeErr)
			}
			return nil, err
		}
		return &fileClosingIO{IO: io, file: f}, nil
	}
}

// containerEntry tracks a container created through the fake Docker socket.
type containerEntry struct {
	ID           string
	Name         string
	Image        string
	Hostname     string
	Cmd          []string
	Env          []string
	Created      time.Time
	Container    client.Container
	Task         client.Task
	LogPath      string
	NetNS        string
	IP           string
	Status       string // "created", "running", "exited"
	ExitCode     uint32
	Networks     map[string]containerNetworkInfo // network name → info
	PortBindings map[string][]portBinding        // container port → host bindings
	Labels       map[string]string
	Tty          bool
}

// createRequest is the subset of Docker's container create body we support.
type createRequest struct {
	Hostname         string            `json:"Hostname"`
	Image            string            `json:"Image"`
	Cmd              []string          `json:"Cmd"`
	Entrypoint       []string          `json:"Entrypoint"`
	Env              []string          `json:"Env"`
	WorkingDir       string            `json:"WorkingDir"`
	Tty              bool              `json:"Tty"`
	Labels           map[string]string `json:"Labels"`
	ExposedPorts     map[string]any    `json:"ExposedPorts"`
	Volumes          map[string]any    `json:"Volumes"`
	HostConfig       *hostConfig       `json:"HostConfig"`
	NetworkingConfig *networkingConfig `json:"NetworkingConfig"`
}

type hostConfig struct {
	Binds         []string                       `json:"Binds"`
	NetworkMode   string                         `json:"NetworkMode"`
	Privileged    bool                           `json:"Privileged"`
	SecurityOpt   []string                       `json:"SecurityOpt"`
	CapAdd        []string                       `json:"CapAdd"`
	Tmpfs         map[string]string              `json:"Tmpfs"`
	PortBindings  map[string][]portBinding        `json:"PortBindings"`
	RestartPolicy *restartPolicy                 `json:"RestartPolicy"`
	Init          *bool                          `json:"Init"`
	CgroupnsMode  string                         `json:"CgroupnsMode"`
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type restartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type networkingConfig struct {
	EndpointsConfig map[string]*endpointSettings `json:"EndpointsConfig"`
}

type endpointSettings struct {
	IPAMConfig *endpointIPAMConfig `json:"IPAMConfig"`
}

type endpointIPAMConfig struct {
	IPv4Address string `json:"IPv4Address"`
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

	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)

	// Resolve image from containerd, pulling if needed. Docker CLI sends
	// unqualified refs (e.g. "moby/buildkit:buildx-stable-1") that
	// containerd's resolver mistakes for "host=moby"; try the original
	// name first (handleImagePull aliases the qualified pull under the
	// unqualified name so this hits when buildx pulled before creating),
	// then fall back to a qualified pull if nothing's there.
	img, err := s.client.GetImage(ctx, req.Image)
	if err != nil {
		qualified := qualifyDockerHubRef(req.Image)
		if qualified != req.Image {
			if alt, gerr := s.client.GetImage(ctx, qualified); gerr == nil {
				img, err = alt, nil
			}
		}
	}
	if err != nil {
		pullRef := qualifyDockerHubRef(req.Image)
		s.log.Info("image not found, pulling for container create", "image", req.Image, "pull_ref", pullRef)
		img, err = s.client.Pull(ctx, pullRef, client.WithPullUnpack)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"message": fmt.Sprintf("image %s not found: %v", req.Image, err),
			})
			return
		}
	}

	// Build OCI spec. Always target Linux — dind containers are Linux.
	//
	// Apply Docker's entrypoint/cmd override semantics on top of the image config:
	//   Entrypoint nil  + Cmd nil   → image ENTRYPOINT + image CMD       (image as-is)
	//   Entrypoint nil  + Cmd set   → image ENTRYPOINT + req.Cmd          (most common, e.g. buildkit)
	//   Entrypoint set + Cmd  any   → req.Entrypoint   + req.Cmd          (full override)
	//
	// WithImageConfigArgs handles the first two: if args is empty it keeps image cmd,
	// otherwise it substitutes args for the image's CMD while preserving ENTRYPOINT.
	// For the override case we still want WithImageConfig's env/cwd/user, so we layer
	// a final SpecOpts that rewrites Process.Args.
	targetPlatform := "linux/" + goruntime.GOARCH
	opts := []oci.SpecOpts{
		oci.WithDefaultSpecForPlatform(targetPlatform),
	}
	if len(req.Entrypoint) > 0 {
		opts = append(opts, oci.WithImageConfig(img))
		merged := append([]string{}, req.Entrypoint...)
		merged = append(merged, req.Cmd...)
		opts = append(opts, oci.WithProcessArgs(merged...))
	} else {
		opts = append(opts, oci.WithImageConfigArgs(img, req.Cmd))
	}

	if len(req.Env) > 0 {
		opts = append(opts, oci.WithEnv(req.Env))
	}

	if req.WorkingDir != "" {
		opts = append(opts, oci.WithProcessCwd(req.WorkingDir))
	}

	if req.Hostname != "" {
		opts = append(opts, oci.WithHostname(req.Hostname))
	}

	// Note: req.Tty is stored on the containerEntry for inspect responses
	// but NOT applied to the OCI spec here. Terminal mode is enabled
	// selectively in handleContainerStart for containers that need a
	// real PTY (e.g. kindest/node, where systemd needs /dev/console).

	if req.HostConfig != nil {
		// Privileged mode: all capabilities, all devices, disable seccomp/apparmor,
		// writable /proc and /sys. Safe because dind containers run inside an
		// isolated Hyper-V VM — privileged only means "root within the VM."
		if req.HostConfig.Privileged {
			opts = append(opts,
				oci.WithPrivileged,
				oci.WithAllDevicesAllowed,
				oci.WithHostDevices,
				oci.WithSeccompUnconfined,
				oci.WithWriteableSysfs,
				oci.WithWriteableCgroupfs,
				oci.WithApparmorProfile(""),
				oci.WithMaskedPaths(nil),
				oci.WithReadonlyPaths(nil),
				oci.WithNewPrivileges,
				withExplicitCgroup2Mount(),
			)
		}

		// Additional capabilities (e.g. --cap-add SYS_ADMIN).
		if len(req.HostConfig.CapAdd) > 0 {
			opts = append(opts, oci.WithAddedCapabilities(req.HostConfig.CapAdd))
		}

		// Security options (seccomp=unconfined, apparmor=unconfined).
		for _, opt := range req.HostConfig.SecurityOpt {
			switch {
			case opt == "seccomp=unconfined" || opt == "seccomp:unconfined":
				opts = append(opts, oci.WithSeccompUnconfined)
			case strings.HasPrefix(opt, "apparmor=") || strings.HasPrefix(opt, "apparmor:"):
				profile := strings.SplitN(opt, "=", 2)
				if len(profile) == 1 {
					profile = strings.SplitN(opt, ":", 2)
				}
				if len(profile) == 2 && profile[1] == "unconfined" {
					opts = append(opts, oci.WithApparmorProfile(""))
				}
			}
		}

		// Private cgroup namespace (--cgroupns=private).
		if req.HostConfig.CgroupnsMode == "private" {
			opts = append(opts, oci.WithNamespacedCgroup())
		}

		// Bind mounts. Skip sources that don't exist rather than failing.
		for _, bind := range req.HostConfig.Binds {
			bindParts := strings.SplitN(bind, ":", 3)
			if len(bindParts) >= 2 {
				src := bindParts[0]
				if _, err := os.Stat(src); os.IsNotExist(err) {
					s.log.Debug("skipping bind mount, source does not exist", "source", src, "dest", bindParts[1])
					continue
				}
				mountOpts := []string{"rbind", "rw"}
				if len(bindParts) == 3 && bindParts[2] == "ro" {
					mountOpts = []string{"rbind", "ro"}
				}
				opts = append(opts, withBindMount(src, bindParts[1], mountOpts))
			}
		}

		// tmpfs mounts (--tmpfs /tmp, --tmpfs /run).
		for dest, options := range req.HostConfig.Tmpfs {
			tmpfsOpts := []string{"nosuid", "nodev"}
			if options != "" {
				tmpfsOpts = strings.Split(options, ",")
			}
			opts = append(opts, withTmpfsMount(dest, tmpfsOpts))
		}
	}

	// For kindest/node containers, wrap the process to pre-register
	// iptables alternatives that may be missing from the overlay. The
	// Debian alternatives database lives in a lower image layer and is
	// sometimes not visible through overlayfs; this init wrapper
	// re-creates it at container start before the real entrypoint runs.
	if strings.Contains(req.Image, "kindest/node") && req.HostConfig != nil && req.HostConfig.Privileged {
		opts = append(opts, withKindNodeInit(s.log))
	}

	// Anonymous volumes (e.g. VOLUME /var in Dockerfile or --volume /var).
	// Real Docker creates a named volume, copies image content into it, then
	// bind-mounts it. We skip them: the overlay upperdir is already writable
	// and contains the merged image content, so mounting empty tmpfs would
	// just hide the image data (e.g. /var/lib/dpkg, systemd units).
	if len(req.Volumes) > 0 {
		dests := make([]string, 0, len(req.Volumes))
		for dest := range req.Volumes {
			dests = append(dests, dest)
		}
		s.log.Info("skipping anonymous volumes (overlay provides content)", "volumes", dests)
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

	var ports map[string][]portBinding
	if req.HostConfig != nil && len(req.HostConfig.PortBindings) > 0 {
		ports = req.HostConfig.PortBindings
	}
	labels := req.Labels
	if labels == nil {
		labels = map[string]string{}
	}

	entry := &containerEntry{
		ID:           id,
		Name:         name,
		Image:        req.Image,
		Hostname:     req.Hostname,
		Cmd:          req.Cmd,
		Env:          req.Env,
		Created:      time.Now(),
		Container:    container,
		Status:       "created",
		Networks:     make(map[string]containerNetworkInfo),
		PortBindings: ports,
		Labels:       labels,
		Tty:          req.Tty,
	}

	s.mu.Lock()
	s.containers[id] = entry
	s.assignContainerNetwork(entry, req)
	s.mu.Unlock()

	s.log.Info("container created", "id", id, "name", name, "image", req.Image)

	// Ensure files from lower overlay layers are visible in the container.
	// Containerd's overlayfs snapshotter can produce mounts where directories
	// present only in lower layers aren't visible to runc's rootfs assembly.
	// Copy key directories from lowerdirs into the upperdir as a workaround.
	s.copyUpMissingPaths(ctx, snapshotName)

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

	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)

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

	ioCreator := cio.LogFile(logPath)
	if strings.Contains(entry.Image, "kindest/node") {
		ioCreator = logFileTerminal(logPath)
	}
	task, err := entry.Container.NewTask(ctx, ioCreator)
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

			// Update the "bridge" network entry with the real CNI-assigned IP.
			s.mu.Lock()
			if info, ok := entry.Networks["bridge"]; ok {
				info.IPAddress = result.IP
				info.MacAddress = generateMAC(result.IP)
				entry.Networks["bridge"] = info
			}
			if br := s.defaultNetwork(); br != nil {
				br.Containers[id] = result.IP
			}
			s.mu.Unlock()
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

	// Monitor for unexpected exit so we can diagnose crashes.
	bgCtx := namespaces.WithNamespace(context.Background(), s.jobNamespace)
	exitCh, waitErr := task.Wait(bgCtx)
	if waitErr == nil {
		go func() {
			status := <-exitCh
			s.log.Warn("container task exited", "id", id[:12], "name", entry.Name,
				"exit_code", status.ExitCode(), "error", status.Error())
			entry.Status = "exited"
			entry.ExitCode = status.ExitCode()
			if entry.LogPath != "" {
				if data, err := os.ReadFile(entry.LogPath); err == nil {
					tail := data
					if len(tail) > 8192 {
						tail = tail[len(tail)-8192:]
					}
					s.log.Info("container log tail on exit", "id", id[:12], "log", string(tail))
				}
			}
		}()
	}

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
		ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)
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

	networks := make(map[string]any, len(entry.Networks))
	for name, info := range entry.Networks {
		networks[name] = map[string]any{
			"IPAMConfig":  nil,
			"NetworkID":   info.NetworkID,
			"EndpointID":  generateContainerID(),
			"Gateway":     info.Gateway,
			"IPAddress":   info.IPAddress,
			"IPPrefixLen": info.PrefixLen,
			"MacAddress":  info.MacAddress,
		}
	}

	primaryIP := entry.IP
	if primaryIP == "" {
		for _, info := range entry.Networks {
			primaryIP = info.IPAddress
			break
		}
	}

	// Build Ports map for NetworkSettings. Docker format:
	// "6443/tcp": [{"HostIp": "127.0.0.1", "HostPort": "37159"}]
	ports := make(map[string]any, len(entry.PortBindings))
	for containerPort, bindings := range entry.PortBindings {
		bindingList := make([]map[string]string, len(bindings))
		for i, b := range bindings {
			bindingList[i] = map[string]string{
				"HostIp":   b.HostIP,
				"HostPort": b.HostPort,
			}
		}
		ports[containerPort] = bindingList
	}

	hostname := entry.Hostname
	if hostname == "" {
		hostname = id[:12]
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
			"Hostname": hostname,
			"Image":    entry.Image,
			"Cmd":      entry.Cmd,
			"Env":      entry.Env,
			"Labels":   entry.Labels,
			"Tty":      entry.Tty,
		},
		"NetworkSettings": map[string]any{
			"IPAddress": primaryIP,
			"Ports":     ports,
			"Networks":  networks,
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

	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)

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

	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)
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

	follow := r.URL.Query().Get("follow") == "true" || r.URL.Query().Get("follow") == "1"

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)

	if !follow {
		data, err := os.ReadFile(entry.LogPath)
		if err != nil {
			if !os.IsNotExist(err) {
				s.log.Debug("reading logs", "error", err)
			}
			return
		}
		if _, err := w.Write(data); err != nil {
			s.log.Debug("writing log response", "error", err)
		}
		return
	}

	// Follow mode: tail the log file until the container exits or client disconnects.
	flusher, canFlush := w.(http.Flusher)
	var offset int64
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			f, err := os.Open(entry.LogPath)
			if err != nil {
				if os.IsNotExist(err) {
					if entry.Status == "exited" {
						return
					}
					continue
				}
				return
			}
			fi, err := f.Stat()
			if err != nil {
				f.Close()
				continue
			}
			if fi.Size() <= offset {
				f.Close()
				if entry.Status == "exited" {
					return
				}
				continue
			}
			if _, err := f.Seek(offset, 0); err != nil {
				f.Close()
				continue
			}
			buf := make([]byte, fi.Size()-offset)
			n, readErr := f.Read(buf)
			f.Close()
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				offset += int64(n)
				if canFlush {
					flusher.Flush()
				}
			}
			if readErr != nil && readErr.Error() != "EOF" {
				return
			}
		}
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

	ctx := namespaces.WithNamespace(r.Context(), s.jobNamespace)
	s.cleanupContainer(ctx, id, entry)

	s.mu.Lock()
	delete(s.containers, id)
	s.removeContainerFromNetworks(id)
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
		networks := make(map[string]any, len(entry.Networks))
		for name, info := range entry.Networks {
			networks[name] = map[string]any{
				"IPAddress": info.IPAddress,
			}
		}
		result = append(result, map[string]any{
			"Id":      entry.ID,
			"Names":   names,
			"Image":   entry.Image,
			"State":   entry.Status,
			"Created": entry.Created.Unix(),
			"NetworkSettings": map[string]any{
				"Networks": networks,
			},
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// assignContainerNetwork assigns a container to the appropriate Docker network
// based on the create request. Must be called with s.mu held.
func (s *Server) assignContainerNetwork(entry *containerEntry, req createRequest) {
	// Determine target network from request.
	if req.NetworkingConfig != nil && len(req.NetworkingConfig.EndpointsConfig) > 0 {
		for netName, epConfig := range req.NetworkingConfig.EndpointsConfig {
			nw := s.resolveNetwork(netName)
			if nw == nil {
				continue
			}
			var ip string
			if epConfig != nil && epConfig.IPAMConfig != nil && epConfig.IPAMConfig.IPv4Address != "" {
				ip = epConfig.IPAMConfig.IPv4Address
			} else {
				var err error
				ip, err = allocateIP(nw)
				if err != nil {
					s.log.Warn("failed to allocate IP for container", "container", entry.ID[:12], "network", netName, "error", err)
					continue
				}
			}
			nw.Containers[entry.ID] = ip
			entry.Networks[nw.Name] = containerNetworkInfo{
				NetworkID:  nw.ID,
				IPAddress:  ip,
				Gateway:    nw.Gateway,
				MacAddress: generateMAC(ip),
				PrefixLen:  prefixLen(nw.Subnet),
			}
			entry.IP = ip
		}
		return
	}

	// Check HostConfig.NetworkMode.
	netName := "bridge"
	if req.HostConfig != nil && req.HostConfig.NetworkMode != "" &&
		req.HostConfig.NetworkMode != "default" && req.HostConfig.NetworkMode != "bridge" {
		netName = req.HostConfig.NetworkMode
	}

	nw := s.resolveNetwork(netName)
	if nw == nil {
		nw = s.defaultNetwork()
	}
	if nw == nil {
		return
	}

	ip, err := allocateIP(nw)
	if err != nil {
		s.log.Warn("failed to allocate IP for container", "container", entry.ID[:12], "error", err)
		return
	}

	nw.Containers[entry.ID] = ip
	entry.Networks[nw.Name] = containerNetworkInfo{
		NetworkID:  nw.ID,
		IPAddress:  ip,
		Gateway:    nw.Gateway,
		MacAddress: generateMAC(ip),
		PrefixLen:  prefixLen(nw.Subnet),
	}
	entry.IP = ip
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
	ctx := namespaces.WithNamespace(context.Background(), s.jobNamespace)

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

// withKindNodeInit wraps the container's process args to pre-register iptables
// alternatives before running the real entrypoint. The overlay's dpkg copyup
// provides the alternatives database files, but the symlink targets may not
// match; this wrapper ensures a valid alternatives DB exists for select_iptables().
func withKindNodeInit(log *slog.Logger) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *ocispec.Spec) error {
		if s.Process == nil || len(s.Process.Args) == 0 {
			return nil
		}

		// Enable terminal mode so runc allocates a real PTY for /dev/console.
		// systemd checks isatty(stdout) and only prints status messages (like
		// "Reached target Multi-User System") when it has a real terminal.
		s.Process.Terminal = true

		origArgs := s.Process.Args
		quoted := make([]string, len(origArgs))
		for i, arg := range origArgs {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
		}

		script := `set -e
# Pre-register iptables alternatives if the database is missing.
# The Debian alternatives DB lives in a lower overlay layer and may
# not be visible; re-create it so select_iptables() in the entrypoint
# finds a working alternatives database.
if ! update-alternatives --display iptables >/dev/null 2>&1; then
  update-alternatives --install /usr/sbin/iptables iptables /usr/sbin/iptables-nft 20 \
    --slave /usr/sbin/iptables-save iptables-save /usr/sbin/iptables-nft-save \
    --slave /usr/sbin/iptables-restore iptables-restore /usr/sbin/iptables-nft-restore 2>&1 || true
  update-alternatives --install /usr/sbin/iptables iptables /usr/sbin/iptables-legacy 10 \
    --slave /usr/sbin/iptables-save iptables-save /usr/sbin/iptables-legacy-save \
    --slave /usr/sbin/iptables-restore iptables-restore /usr/sbin/iptables-legacy-restore 2>&1 || true
fi
if ! update-alternatives --display ip6tables >/dev/null 2>&1; then
  update-alternatives --install /usr/sbin/ip6tables ip6tables /usr/sbin/ip6tables-nft 20 \
    --slave /usr/sbin/ip6tables-save ip6tables-save /usr/sbin/ip6tables-nft-save \
    --slave /usr/sbin/ip6tables-restore ip6tables-restore /usr/sbin/ip6tables-nft-restore 2>&1 || true
  update-alternatives --install /usr/sbin/ip6tables ip6tables /usr/sbin/ip6tables-legacy 10 \
    --slave /usr/sbin/ip6tables-save ip6tables-save /usr/sbin/ip6tables-legacy-save \
    --slave /usr/sbin/ip6tables-restore ip6tables-restore /usr/sbin/ip6tables-legacy-restore 2>&1 || true
fi
exec ` + strings.Join(quoted, " ")

		log.Info("wrapping kindest/node process with init script", "original_args", origArgs)
		s.Process.Args = []string{"/bin/bash", "-c", script}
		return nil
	}
}

// withExplicitCgroup2Mount replaces the generic "cgroup" mount with an explicit
// cgroup2 mount. The default OCI spec uses type "cgroup" which runc resolves at
// runtime, but inside our minimal VM this can misdetect as v1.
func withExplicitCgroup2Mount() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *ocispec.Spec) error {
		for i, m := range s.Mounts {
			if m.Destination == "/sys/fs/cgroup" {
				s.Mounts[i] = ocispec.Mount{
					Destination: "/sys/fs/cgroup",
					Type:        "cgroup2",
					Source:      "cgroup2",
					Options:     []string{"rw", "nosuid", "nodev", "noexec"},
				}
				return nil
			}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup2",
			Source:      "cgroup2",
			Options:     []string{"rw", "nosuid", "nodev", "noexec"},
		})
		return nil
	}
}

// copyUpMissingPaths copies /var/lib/dpkg from lower overlay layers into
// the upperdir so the Debian alternatives database is visible to containers.
// This is a safety net for images where the alternatives DB lives only in a
// lower layer and overlayfs doesn't merge it into the container view.
func (s *Server) copyUpMissingPaths(ctx context.Context, snapshotName string) {
	snapshotter := s.client.SnapshotService("overlayfs")
	if snapshotter == nil {
		return
	}
	mounts, err := snapshotter.Mounts(ctx, snapshotName)
	if err != nil {
		return
	}

	var upperDir string
	var lowerDirs []string
	for _, m := range mounts {
		for _, opt := range m.Options {
			for _, part := range strings.Split(opt, ",") {
				if strings.HasPrefix(part, "upperdir=") {
					upperDir = strings.TrimPrefix(part, "upperdir=")
				}
				if strings.HasPrefix(part, "lowerdir=") {
					lowerDirs = strings.Split(strings.TrimPrefix(part, "lowerdir="), ":")
				}
			}
		}
	}
	if upperDir == "" || len(lowerDirs) == 0 {
		return
	}

	for i := len(lowerDirs) - 1; i >= 0; i-- {
		src := filepath.Join(lowerDirs[i], "var/lib/dpkg")
		if _, statErr := os.Stat(src); statErr != nil {
			continue
		}
		dst := filepath.Join(upperDir, "var/lib/dpkg")
		if _, statErr := os.Stat(dst); statErr == nil {
			continue
		}
		s.log.Info("copyup: copying dpkg from layer to upper", "idx", i, "src", src, "dst", dst)
		if cpErr := copyDirRecursive(src, dst); cpErr != nil {
			s.log.Warn("copyup: failed to copy dpkg", "error", cpErr)
		}
		break
	}
}

func copyDirRecursive(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

func withTmpfsMount(dst string, options []string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *ocispec.Spec) error {
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: dst,
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     options,
		})
		return nil
	}
}

