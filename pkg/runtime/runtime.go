package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	goruntime "runtime"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/dind"
	"github.com/ephpm/ephemerd/pkg/networking"
	craneTarball "github.com/google/go-containerregistry/pkg/v1/tarball"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	namespace         = "ephemerd"
	defaultImageLinux = "ghcr.io/actions/actions-runner:latest"
)

// containerCapabilities is the minimum set of Linux capabilities for CI jobs.
// Covers apt-get install, sudo, adduser, and service management.
// Docker-in-Docker is not supported — use Kaniko or Buildah for image builds.
var containerCapabilities = []string{
	"CAP_CHOWN",            // dpkg chown on installed files
	"CAP_DAC_OVERRIDE",     // write to dirs owned by other users
	"CAP_FOWNER",           // chmod/utimes on files not owned by process
	"CAP_FSETID",           // preserve SUID/SGID bits (sudo, passwd)
	"CAP_KILL",             // signal processes (postinst service restarts)
	"CAP_SETGID",           // adduser/addgroup in maintainer scripts
	"CAP_SETUID",           // setuid in maintainer scripts
	"CAP_MKNOD",            // create device nodes (some packages)
	"CAP_SYS_CHROOT",       // chroot in maintainer scripts
	"CAP_NET_BIND_SERVICE", // bind to ports < 1024
}

// Config for the container runtime.
type Config struct {
	Client       *client.Client
	RunnerDir    string // host path to extracted runner binary
	RunnerMount  string // container path to mount runner at
	DefaultImage string // override default container image (auto-detected if empty)
	ImagesDir    string // directory containing pre-downloaded OCI image tarballs to import on startup
	LogDir       string // directory for per-job container logs
	DataDir      string // ephemerd data directory (used for dind socket paths)
	// ContainerDataDir is the path containerd/runc see for the DataDir.
	// On Linux this matches DataDir. On Darwin the host DataDir is shared
	// into the Linux VM via virtio-fs at a different path (e.g.
	// /mnt/ephemerd), and any bind-mount sources that reference the
	// DataDir must be rewritten to that VM-side path. When empty, falls
	// back to DataDir.
	ContainerDataDir string
	DindEnabled      bool     // mount a fake Docker socket into each container
	CacheProxyEnv    []string // extra env vars from cache proxies (e.g., GOPROXY=...)
	Network          *networking.Manager
	Log              *slog.Logger
}

// Runtime manages container lifecycle for runner environments.
type Runtime struct {
	cfg    Config
	client *client.Client
	pullMu sync.Mutex // serializes image pulls to avoid content store contention
}

// RunnerEnv represents a running runner environment.
type RunnerEnv struct {
	ID        string
	Netns     string // network namespace path (Linux only)
	RunnerDir string // per-job runner copy, cleaned up on destroy
	Dind      *dind.Server // per-job fake Docker daemon (nil if disabled)
	Container client.Container
	Task      client.Task
}

// New creates a container runtime manager.
func New(cfg Config) (*Runtime, error) {
	return &Runtime{
		cfg:    cfg,
		client: cfg.Client,
	}, nil
}

// LogDir returns the configured per-job log directory (empty if logs go to stdio).
func (r *Runtime) LogDir() string {
	return r.cfg.LogDir
}

// CleanOrphans removes any leftover containers and snapshots from a previous
// ephemerd run. This should be called on startup before the scheduler starts
// accepting jobs.
func (r *Runtime) CleanOrphans(ctx context.Context) error {
	ctx = namespaces.WithNamespace(ctx, namespace)

	// Clean orphan containers (and their associated snapshots)
	containers, err := r.client.Containers(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	if len(containers) > 0 {
		r.cfg.Log.Info("cleaning orphan containers", "count", len(containers))
	}

	for _, c := range containers {
		id := c.ID()
		log := r.cfg.Log.With("id", id)

		// Try to kill and delete the task in any state
		task, err := c.Task(ctx, nil)
		if err == nil {
			st, err := task.Status(ctx)
			if err == nil {
				log.Debug("orphan task state", "status", st.Status)
				if st.Status == client.Running {
					if err := task.Kill(ctx, 9); err != nil {
						log.Debug("failed to kill orphan task", "error", err)
					}
					exitCh, err := task.Wait(ctx)
					if err == nil {
						<-exitCh
					}
				}
			}
			// WithProcessKill forces deletion even if task is in created state
			if _, err := task.Delete(ctx, client.WithProcessKill); err != nil {
				log.Debug("failed to delete orphan task", "error", err)
			}
		}

		// Delete container and snapshot
		if err := c.Delete(ctx, client.WithSnapshotCleanup); err != nil {
			log.Warn("failed to delete orphan container", "error", err)
		} else {
			log.Info("orphan container removed")
		}
	}

	// Clean orphan snapshots that no longer have a container pointing to them.
	// This catches snapshots left behind when a container create partially failed.
	snapshotter := r.client.SnapshotService("overlayfs")
	if snapshotter == nil {
		return nil
	}

	containerIDs := make(map[string]bool, len(containers))
	for _, c := range containers {
		containerIDs[c.ID()+"-snapshot"] = true
	}

	return snapshotter.Walk(ctx, func(snapCtx context.Context, info snapshots.Info) error {
		// Only clean ephemerd snapshots (they all end with -snapshot)
		if !strings.HasSuffix(info.Name, "-snapshot") {
			return nil
		}
		// Skip if we already handled it via container delete above
		if containerIDs[info.Name] {
			return nil
		}
		r.cfg.Log.Info("removing orphan snapshot", "name", info.Name)
		if err := snapshotter.Remove(ctx, info.Name); err != nil {
			r.cfg.Log.Warn("failed to remove orphan snapshot", "name", info.Name, "error", err)
		}
		return nil
	})
}

// ImportImages loads pre-downloaded OCI image tarballs from the images directory.
// Each tarball is inspected for its target OS. Images matching the host OS are
// imported into the host containerd and unpacked immediately. Images targeting a
// different OS (e.g. Linux images on a Windows host) are returned as deferred
// paths — the caller should import them into the appropriate VM's containerd
// after the VM is ready using ImportImagesTo.
//
// On Linux, all images are imported directly (no deferral).
func (r *Runtime) ImportImages(ctx context.Context) (deferred []string, err error) {
	dir := r.cfg.ImagesDir
	if dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading images dir %s: %w", dir, err)
	}

	hostOS := goruntime.GOOS
	snapshotter := "overlayfs"
	if hostOS == "windows" {
		snapshotter = "windows"
	}

	ctx = namespaces.WithNamespace(ctx, namespace)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tar") {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		// Inspect the tarball to determine the image's target OS.
		imageOS, err := tarballImageOS(path)
		if err != nil {
			r.cfg.Log.Warn("could not detect image OS, importing to host", "path", path, "error", err)
			// Fall through and try to import — worst case it fails to unpack.
		} else if imageOS != hostOS {
			r.cfg.Log.Info("deferring image for VM import", "path", path, "imageOS", imageOS, "hostOS", hostOS)
			deferred = append(deferred, path)
			continue
		}

		if importErr := importTarball(ctx, r.client, path, snapshotter, r.cfg.Log); importErr != nil {
			r.cfg.Log.Warn("failed to import image", "path", path, "error", importErr)
		}
	}

	return deferred, nil
}

// ImportImagesTo imports a list of OCI image tarballs into the given containerd
// client. Used to import deferred Linux images into a VM's containerd after the
// VM is ready.
func ImportImagesTo(ctx context.Context, c *client.Client, paths []string, snapshotter string, log *slog.Logger) {
	ctx = namespaces.WithNamespace(ctx, namespace)
	for _, path := range paths {
		if err := importTarball(ctx, c, path, snapshotter, log); err != nil {
			log.Warn("failed to import image to VM", "path", path, "error", err)
		}
	}
}

// importTarball imports a single OCI tarball into a containerd client and unpacks it.
func importTarball(ctx context.Context, c *client.Client, path, snapshotter string, log *slog.Logger) error {
	log.Info("importing image from tarball", "path", path)

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}

	imgs, err := c.Import(ctx, f)
	if closeErr := f.Close(); closeErr != nil {
		log.Warn("error closing image tarball", "path", path, "error", closeErr)
	}
	if err != nil {
		return fmt.Errorf("importing %s: %w", path, err)
	}

	for _, img := range imgs {
		log.Info("imported image, unpacking", "name", img.Name, "snapshotter", snapshotter)

		cImg, err := c.GetImage(ctx, img.Name)
		if err != nil {
			log.Warn("failed to get imported image for unpack", "name", img.Name, "error", err)
			continue
		}
		if err := cImg.Unpack(ctx, snapshotter); err != nil {
			log.Warn("failed to unpack imported image", "name", img.Name, "error", err)
			continue
		}
		log.Info("image imported and unpacked", "name", img.Name)
	}
	return nil
}

// tarballImageOS reads an OCI/Docker image tarball and returns the OS of the
// first image found (e.g. "linux", "windows"). Uses go-containerregistry to
// parse the tarball metadata without extracting it.
func tarballImageOS(path string) (string, error) {
	img, err := craneTarball.ImageFromPath(path, nil)
	if err != nil {
		return "", fmt.Errorf("reading tarball %s: %w", path, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return "", fmt.Errorf("reading config from %s: %w", path, err)
	}
	if cfg.OS == "" {
		return "", fmt.Errorf("no OS in image config for %s", path)
	}
	return cfg.OS, nil
}

// PullImage ensures the runner image is available locally.
// Serialized with a mutex to avoid concurrent pulls contending on
// the content store (which produces noisy lock errors).
func (r *Runtime) PullImage(ctx context.Context, ref string) error {
	r.pullMu.Lock()
	defer r.pullMu.Unlock()

	ctx = namespaces.WithNamespace(ctx, namespace)

	// Check if another goroutine already pulled it while we waited
	if _, err := r.client.GetImage(ctx, ref); err == nil {
		return nil
	}

	r.cfg.Log.Info("pulling image", "ref", ref)

	// Force overlayfs snapshotter. containerd 2.2.2 may default to the
	// experimental erofs snapshotter on some Linux kernels, which then
	// fails image unpack with "snapshotter not loaded: erofs: invalid argument".
	snapshotter := "overlayfs"
	pullOpts := []client.RemoteOpt{
		client.WithPullUnpack,
	}
	if goruntime.GOOS == "windows" {
		snapshotter = "windows"
		// Windows container images are multi-platform manifests. Without an
		// explicit platform filter, containerd may attempt to resolve the
		// wrong OS version or download all variants, causing the pull to hang.
		pullOpts = append(pullOpts, client.WithPlatform("windows/amd64"))
	}
	pullOpts = append(pullOpts, client.WithPullSnapshotter(snapshotter))
	_, err := r.client.Pull(ctx, ref, pullOpts...)
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", ref, err)
	}

	r.cfg.Log.Info("image ready", "ref", ref)
	return nil
}

// CreateConfig holds parameters for creating a runner environment.
type CreateConfig struct {
	ID    string // unique job identifier (container name, dind socket path)
	Image string // OCI image reference (empty = use default)

	// JITConfig is the base64-encoded JIT config for GitHub runners.
	// Passed as "--jitconfig <value>" to the runner entrypoint.
	// Mutually exclusive with Entrypoint.
	JITConfig string

	// Env holds extra environment variables injected into the container.
	// Used by Gitea/Forgejo to pass instance URL, runner token, etc.
	Env map[string]string

	// Entrypoint overrides the container's process args.
	// When set, used instead of the default "--jitconfig" entrypoint.
	// When nil and JITConfig is set, uses the GitHub "--jitconfig" mode.
	// When nil and JITConfig is empty, uses the image's default CMD.
	Entrypoint []string
}

// Create provisions an ephemeral runner environment.
func (r *Runtime) Create(ctx context.Context, cfg CreateConfig) (*RunnerEnv, error) {
	id := cfg.ID
	image := cfg.Image
	jitConfig := cfg.JITConfig
	ctx = namespaces.WithNamespace(ctx, namespace)

	// Use a default image when no custom image is specified.
	// If runner.default_image is set in config, use that.
	// Otherwise: Linux uses the official GHA runner image,
	// Windows auto-selects a Server Core image matching the host OS build.
	customImage := image != ""
	if !customImage {
		if r.cfg.DefaultImage != "" {
			image = r.cfg.DefaultImage
		} else {
			image = defaultImage()
		}
	}

	r.cfg.Log.Info("creating runner environment", "id", id, "image", image, "custom", customImage)

	// Get the image, pulling it if not present locally
	img, err := r.client.GetImage(ctx, image)
	if err != nil {
		r.cfg.Log.Info("image not found locally, pulling", "image", image)
		if err := r.PullImage(ctx, image); err != nil {
			return nil, fmt.Errorf("pulling image %s: %w", image, err)
		}
		img, err = r.client.GetImage(ctx, image)
		if err != nil {
			return nil, fmt.Errorf("getting image %s after pull: %w", image, err)
		}
	}

	// Runner paths differ: official image has runner at /home/runner,
	// custom images get our embedded runner mounted at /actions-runner.
	var entrypoint string
	if goruntime.GOOS == "windows" {
		entrypoint = `C:\actions-runner\run.cmd`
	} else if customImage {
		entrypoint = "/actions-runner/run.sh"
	} else {
		entrypoint = "/home/runner/run.sh"
	}

	// Build container spec. containerd's default spec generator uses the HOST
	// GOOS to decide whether to populate the Linux or Windows section of the
	// OCI spec. On macOS hosts that means neither section is filled (the host
	// is darwin), and runc rejects the resulting spec with "spec does not
	// contain Linux or Windows section". Force a platform-appropriate base.
	targetPlatform := "linux/" + goruntime.GOARCH
	if goruntime.GOOS == "windows" {
		targetPlatform = "windows/" + goruntime.GOARCH
	}
	envVars := []string{"RUNNER_ALLOW_RUNASROOT=1"}
	envVars = append(envVars, r.cfg.CacheProxyEnv...)
	for k, v := range cfg.Env {
		envVars = append(envVars, k+"="+v)
	}
	opts := []oci.SpecOpts{
		oci.WithDefaultSpecForPlatform(targetPlatform),
		oci.WithImageConfig(img),
		oci.WithEnv(envVars),
		// Allow sudo inside the container. The default OCI spec sets
		// NoNewPrivileges=true which blocks privilege escalation, but
		// jobs need sudo for apt-get install and similar operations.
		oci.WithNewPrivileges,
		// Restrict capabilities to the minimum needed for CI jobs.
		// This covers apt-get install, adduser, sudo, and service management.
		// Docker-in-Docker is not supported (no CAP_SYS_ADMIN/CAP_NET_ADMIN).
		oci.WithCapabilities(containerCapabilities),
	}
	opts = append(opts, seccompOpts()...)
	switch {
	case len(cfg.Entrypoint) > 0:
		// Forge mode: custom entrypoint (e.g. act_runner register + daemon).
		opts = append(opts, oci.WithProcessArgs(cfg.Entrypoint...))
	case jitConfig != "" && goruntime.GOOS == "windows":
		// GitHub on Windows: wrap in cmd.exe redirect for log capture.
		cmdLine := fmt.Sprintf(`%s --jitconfig %s > C:\actions-runner\runner.log 2>&1`, entrypoint, jitConfig)
		opts = append(opts, oci.WithProcessArgs("cmd.exe", "/c", cmdLine))
	case jitConfig != "":
		// GitHub on Linux/macOS: pass JIT config directly.
		opts = append(opts, oci.WithProcessArgs(entrypoint, "--jitconfig", jitConfig))
	}
	// else: no entrypoint override — use image default CMD/ENTRYPOINT.

	// Mount the embedded runner binary into the container.
	// On Linux with the official GHA image, the runner is pre-installed so no mount needed.
	// On Windows, always mount because there's no Windows GHA runner image.
	needsRunnerMount := (customImage || goruntime.GOOS == "windows") && r.cfg.RunnerDir != "" && r.cfg.RunnerMount != ""
	var jobRunnerDir string
	if needsRunnerMount {
		jobRunnerDir = filepath.Join(filepath.Dir(r.cfg.RunnerDir), "job-"+id)
		if err := copyDirForJob(r.cfg.RunnerDir, jobRunnerDir); err != nil {
			return nil, fmt.Errorf("copying runner dir for %s: %w", id, err)
		}
		opts = append(opts, withRunnerMount(jobRunnerDir, r.cfg.RunnerMount))
	}

	// Mount host DNS config so containers can resolve names.
	// filepath.Dir(LogDir) is the DataDir for Linux hosts; the caller
	// (scheduler) also set ContainerDataDir for Darwin so the container
	// sees the virtio-fs-shared path instead of the host path.
	if goruntime.GOOS != "windows" {
		hostDataDir := filepath.Dir(r.cfg.LogDir)
		containerDataDir := hostDataDir
		if r.cfg.ContainerDataDir != "" {
			containerDataDir = r.cfg.ContainerDataDir
		}
		opts = append(opts, withDNSMount(hostDataDir, containerDataDir, id))
	}

	// Start per-job fake Docker daemon and mount socket into container
	var dindServer *dind.Server
	if r.cfg.DindEnabled && goruntime.GOOS != "windows" {
		var err error
		dindServer, err = dind.New(dind.Config{
			JobID:   id,
			DataDir: r.cfg.DataDir,
			Client:  r.client,
			Log:     r.cfg.Log,
		})
		if err != nil {
			return nil, fmt.Errorf("creating dind server for %s: %w", id, err)
		}
		if err := dindServer.Start(); err != nil {
			return nil, fmt.Errorf("starting dind server for %s: %w", id, err)
		}
		opts = append(opts, withDockerSocket(dindServer.SocketPath()))
	}

	// Add Hyper-V isolation on Windows
	if goruntime.GOOS == "windows" {
		opts = append(opts, withHyperVIsolation())
	}

	// On Windows, create HCN endpoint + namespace before the container so
	// we can add them to the OCI spec. Hyper-V isolated containers require
	// a pre-created network namespace with the endpoint attached.
	var windowsEndpointID, windowsNetNS string
	if goruntime.GOOS == "windows" && r.cfg.Network != nil {
		result, err := r.cfg.Network.Setup(ctx, id, "")
		if err != nil {
			r.cfg.Log.Warn("failed to setup Windows network endpoint", "id", id, "error", err)
		} else if result != nil {
			windowsEndpointID = result.EndpointID
			windowsNetNS = result.NetNS
			opts = append(opts, withWindowsNetwork(windowsNetNS, windowsEndpointID))
		}
	}

	snapshotName := id + "-snapshot"

	// Clean up stale snapshot from a previous failed attempt
	snapshotterName := "overlayfs"
	if goruntime.GOOS == "windows" {
		snapshotterName = "windows"
	}
	snapshotter := r.client.SnapshotService(snapshotterName)
	if snapshotter != nil {
		if _, err := snapshotter.Stat(ctx, snapshotName); err == nil {
			r.cfg.Log.Info("removing stale snapshot before create", "name", snapshotName)
			if err := snapshotter.Remove(ctx, snapshotName); err != nil {
				r.cfg.Log.Warn("failed to remove stale snapshot", "name", snapshotName, "error", err)
			}
		}
	}

	// stopDind is a cleanup helper — safe to call if dindServer is nil.
	stopDind := func() {
		if dindServer != nil {
			dindServer.Stop()
		}
	}

	// Force runc runtime. containerd 2.2 may default to the experimental
	// io.containerd.nerdbox.v1 runtime, whose shim binary isn't in our
	// embed. Use runc explicitly on Linux, host shim on Windows.
	runtimeName := "io.containerd.runc.v2"
	if goruntime.GOOS == "windows" {
		runtimeName = "io.containerd.runhcs.v1"
	}
	container, err := r.client.NewContainer(ctx, id,
		client.WithImage(img),
		client.WithSnapshotter(snapshotterName),
		client.WithNewSnapshot(snapshotName, img),
		client.WithNewSpec(opts...),
		client.WithRuntime(runtimeName, nil),
	)
	if err != nil {
		stopDind()
		// Clean up HCN endpoint + namespace on Windows
		if windowsEndpointID != "" && r.cfg.Network != nil {
			if tearErr := r.cfg.Network.Teardown(ctx, id, windowsNetNS); tearErr != nil {
				r.cfg.Log.Debug("endpoint cleanup after failed container create", "error", tearErr)
			}
		}
		// Clean up snapshot if container creation partially succeeded
		if snapshotter != nil {
			if rmErr := snapshotter.Remove(ctx, snapshotName); rmErr != nil {
				r.cfg.Log.Debug("snapshot cleanup after failed create", "error", rmErr)
			}
		}
		return nil, fmt.Errorf("creating container %s: %w", id, err)
	}

	// Create and start the task with per-job log capture.
	// On Windows, cio.LogFile uses file:// URIs which runhcs rejects
	// (it only accepts binary:// scheme), and cio.WithStdio fails with
	// Access Denied on named pipes. Use NullIO on Windows for now.
	var creator cio.Creator
	if goruntime.GOOS == "windows" {
		creator = cio.NullIO
	} else if r.cfg.LogDir != "" {
		if err := os.MkdirAll(r.cfg.LogDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating log dir: %w", err)
		}
		logPath := filepath.Join(r.cfg.LogDir, id+".log")
		creator = cio.LogFile(logPath)
		r.cfg.Log.Debug("container logs", "id", id, "path", logPath)
	} else {
		creator = cio.NewCreator(cio.WithStdio)
	}
	task, err := container.NewTask(ctx, creator)
	if err != nil {
		stopDind()
		// Clean up HCN endpoint + namespace on Windows
		if windowsEndpointID != "" && r.cfg.Network != nil {
			if tearErr := r.cfg.Network.Teardown(ctx, id, windowsNetNS); tearErr != nil {
				r.cfg.Log.Debug("endpoint cleanup after failed task create", "error", tearErr)
			}
		}
		if delErr := container.Delete(ctx, client.WithSnapshotCleanup); delErr != nil {
			r.cfg.Log.Debug("container cleanup after failed task create", "error", delErr)
		}
		return nil, fmt.Errorf("creating task for %s: %w", id, err)
	}

	// Attach CNI networking before starting the task
	var netns string
	if r.cfg.Network != nil && goruntime.GOOS != "windows" {
		pid := task.Pid()
		netns = fmt.Sprintf("/proc/%d/ns/net", pid)
		if _, err := r.cfg.Network.Setup(ctx, id, netns); err != nil {
			stopDind()
			if _, delErr := task.Delete(ctx, client.WithProcessKill); delErr != nil {
				r.cfg.Log.Debug("task cleanup after failed network setup", "error", delErr)
			}
			if delErr := container.Delete(ctx, client.WithSnapshotCleanup); delErr != nil {
				r.cfg.Log.Debug("container cleanup after failed network setup", "error", delErr)
			}
			return nil, fmt.Errorf("setting up network for %s: %w", id, err)
		}
	}

	if err := task.Start(ctx); err != nil {
		stopDind()
		teardownNetNS := netns
		if windowsNetNS != "" {
			teardownNetNS = windowsNetNS
		}
		if r.cfg.Network != nil && (netns != "" || windowsEndpointID != "") {
			if tearErr := r.cfg.Network.Teardown(ctx, id, teardownNetNS); tearErr != nil {
				r.cfg.Log.Debug("network teardown after failed start", "error", tearErr)
			}
		}
		if _, delErr := task.Delete(ctx, client.WithProcessKill); delErr != nil {
			r.cfg.Log.Debug("task cleanup after failed start", "error", delErr)
		}
		if delErr := container.Delete(ctx, client.WithSnapshotCleanup); delErr != nil {
			r.cfg.Log.Debug("container cleanup after failed start", "error", delErr)
		}
		return nil, fmt.Errorf("starting task for %s: %w", id, err)
	}

	r.cfg.Log.Info("runner environment started", "id", id)

	// On Windows, use the HCN namespace ID for teardown
	envNetns := netns
	if windowsNetNS != "" {
		envNetns = windowsNetNS
	}

	return &RunnerEnv{
		ID:        id,
		Netns:     envNetns,
		RunnerDir: jobRunnerDir,
		Dind:      dindServer,
		Container: container,
		Task:      task,
	}, nil
}

// Destroy tears down a runner environment completely.
func (r *Runtime) Destroy(ctx context.Context, env *RunnerEnv) error {
	ctx = namespaces.WithNamespace(ctx, namespace)

	r.cfg.Log.Info("destroying runner environment", "id", env.ID)

	// Kill the task if still running
	status, err := env.Task.Status(ctx)
	if err == nil && status.Status == client.Running {
		if err := env.Task.Kill(ctx, 9); err != nil {
			r.cfg.Log.Warn("failed to kill task", "id", env.ID, "error", err)
		}
		exitCh, err := env.Task.Wait(ctx)
		if err == nil {
			<-exitCh
		}
	}

	// Delete task
	if _, err := env.Task.Delete(ctx); err != nil {
		r.cfg.Log.Warn("failed to delete task", "id", env.ID, "error", err)
	}

	// Teardown networking (CNI on Linux, HCN endpoint on Windows)
	if r.cfg.Network != nil {
		if env.Netns != "" || goruntime.GOOS == "windows" {
			if err := r.cfg.Network.Teardown(ctx, env.ID, env.Netns); err != nil {
				r.cfg.Log.Warn("failed to teardown network", "id", env.ID, "error", err)
			}
		}
	}

	// Stop fake Docker daemon
	if env.Dind != nil {
		env.Dind.Stop()
	}

	// Delete container and snapshot
	if err := env.Container.Delete(ctx, client.WithSnapshotCleanup); err != nil {
		r.cfg.Log.Warn("failed to delete container", "id", env.ID, "error", err)
	}

	// Clean up per-job runner directory copy
	if env.RunnerDir != "" {
		if err := os.RemoveAll(env.RunnerDir); err != nil {
			r.cfg.Log.Warn("failed to remove job runner dir", "id", env.ID, "path", env.RunnerDir, "error", err)
		}
	}

	r.cfg.Log.Info("runner environment destroyed", "id", env.ID)
	return nil
}

// Wait blocks until the runner environment's task exits.
// Returns the exit status code.
func (r *Runtime) Wait(ctx context.Context, env *RunnerEnv) (uint32, error) {
	ctx = namespaces.WithNamespace(ctx, namespace)

	exitCh, err := env.Task.Wait(ctx)
	if err != nil {
		return 1, fmt.Errorf("waiting for task %s: %w", env.ID, err)
	}

	select {
	case status := <-exitCh:
		return status.ExitCode(), status.Error()
	case <-ctx.Done():
		return 1, ctx.Err()
	}
}

// withDNSMount creates a resolv.conf for the container.
// We write a temporary file with the host's nameservers, filtering out
// any private/unreachable IPs (e.g. WSL2's 10.255.255.254) and falling
// back to public DNS if no usable nameservers are found.
//
// hostDir is where the file is written (where ephemerd can reach it);
// containerSrc is the path the container runtime will see. On Linux/Windows
// these are the same; on Darwin the DataDir is shared into the VM via
// virtio-fs so the container sees a different path.
func withDNSMount(hostDir, containerDir, containerID string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		content := buildResolvConf()

		dir := filepath.Join(hostDir, "dns")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating dns dir: %w", err)
		}
		hostFile := filepath.Join(dir, containerID+".conf")
		if err := os.WriteFile(hostFile, []byte(content), 0o644); err != nil {
			return fmt.Errorf("writing resolv.conf: %w", err)
		}

		src := filepath.Join(containerDir, "dns", containerID+".conf")
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: "/etc/resolv.conf",
			Type:        "bind",
			Source:      src,
			Options:     []string{"rbind", "ro"},
		})
		return nil
	}
}

// buildResolvConf reads the host's resolv.conf and filters out private
// nameservers that containers can't reach. Falls back to public DNS.
func buildResolvConf() string {
	hostConf, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"
	}

	var lines []string
	hasNameserver := false
	for _, line := range strings.Split(string(hostConf), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "nameserver") {
			// Extract the IP and check if it's routable from containers
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 && isRoutableDNS(parts[1]) {
				lines = append(lines, trimmed)
				hasNameserver = true
			}
		} else if strings.HasPrefix(trimmed, "search") || strings.HasPrefix(trimmed, "options") {
			lines = append(lines, trimmed)
		}
	}

	if !hasNameserver {
		lines = append([]string{"nameserver 1.1.1.1", "nameserver 8.8.8.8"}, lines...)
	}

	return strings.Join(lines, "\n") + "\n"
}

// isRoutableDNS checks if a DNS server IP is reachable from containers.
// We only filter out loopback and link-local. Other private IPs (like the
// Hyper-V Default Switch gateway at 172.20.x.1) are reachable because
// containers route through the VM which NATs to the host network.
func isRoutableDNS(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return true // non-IP strings pass through
	}
	// Block loopback, link-local, and RFC1918 private ranges.
	// Containers should only use public DNS servers.
	if parsed.IsLoopback() || parsed.IsLinkLocalUnicast() || parsed.IsPrivate() {
		return false
	}
	return true
}

// withDockerSocket bind-mounts the fake Docker daemon socket into the container.
func withDockerSocket(hostSocketPath string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: "/var/run/docker.sock",
			Type:        "bind",
			Source:      hostSocketPath,
			Options:     []string{"rbind", "rw"},
		})
		return nil
	}
}

// withRunnerMount bind-mounts a per-job copy of the runner directory into the container.
// The runner needs write access (e.g. run-helper.sh at startup) so we can't use
// the shared extracted dir directly. The caller provides a job-specific copy.
func withRunnerMount(hostDir, containerDir string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		if goruntime.GOOS == "windows" {
			// Windows containers use mapped directories, not Linux bind mounts
			s.Mounts = append(s.Mounts, ocispec.Mount{
				Destination: containerDir,
				Source:      hostDir,
				Options:     []string{"rw"},
			})
		} else {
			s.Mounts = append(s.Mounts, ocispec.Mount{
				Destination: containerDir,
				Type:        "bind",
				Source:      hostDir,
				Options:     []string{"rbind", "rw"},
			})
		}
		return nil
	}
}

// copyDirForJob creates a writable copy of src at dst for a single job.
// On Linux, uses hardlinks (cp -al) for instant, space-efficient copies.
// On Windows, uses xcopy.
func copyDirForJob(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if goruntime.GOOS == "windows" {
		return exec.Command("xcopy", src, dst, "/E", "/I", "/Q", "/Y").Run()
	}
	return exec.Command("cp", "-al", src, dst).Run()
}

// withHyperVIsolation is a spec option that enables Hyper-V isolation on Windows.
func withHyperVIsolation() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Windows == nil {
			s.Windows = &ocispec.Windows{}
		}
		s.Windows.HyperV = &ocispec.WindowsHyperV{}
		return nil
	}
}

// withWindowsNetwork configures networking for a Windows container.
// Sets the NetworkNamespace (required for Hyper-V isolated containers)
// and the EndpointList for runhcs to attach the network.
func withWindowsNetwork(namespaceID, endpointID string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Windows == nil {
			s.Windows = &ocispec.Windows{}
		}
		if s.Windows.Network == nil {
			s.Windows.Network = &ocispec.WindowsNetwork{}
		}
		s.Windows.Network.NetworkNamespace = namespaceID
		s.Windows.Network.EndpointList = append(s.Windows.Network.EndpointList, endpointID)
		return nil
	}
}
