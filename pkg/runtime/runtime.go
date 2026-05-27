package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/buildkit"
	"github.com/ephpm/ephemerd/pkg/config"
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
	DindEnabled      bool // mount a fake Docker socket into each container
	// DindAllowPrivileged is forwarded to each per-job dind.Server.
	// When false, requests carrying HostConfig.Privileged=true or
	// HostConfig.CapAdd are rejected with HTTP 403. See
	// config.DindConfig.AllowPrivileged for the threat model.
	DindAllowPrivileged bool
	CacheProxyEnv       []string // extra env vars from cache proxies (e.g., GOPROXY=...)
	// Rlimits sets POSIX resource limits on each runner container's OCI
	// process. Zero values fall back to the containerd default (1024).
	// Applies on Linux only; ignored on Windows (HCS uses a different model).
	Rlimits config.RuntimeRlimits
	Network *networking.Manager
	// WindowsMemoryBytes is the memory limit for Hyper-V isolated Windows
	// runner containers. Zero leaves the OCI spec field unset, which gives
	// the HCS default (~1 GB) — too small for MSVC builds. Caller should
	// pass config.WindowsRunnerToml.MemoryBytes() which defaults to 4 GB.
	WindowsMemoryBytes uint64
	// WindowsCPUs is the virtual CPU count for Hyper-V isolated Windows
	// runner containers. Zero leaves the OCI spec field unset. Caller
	// should pass config.WindowsRunnerToml.CPUCount() which defaults to 2.
	WindowsCPUs uint64
	// BuildKit is the shared embedded BuildKit solver handed to each per-job
	// dind.Server for `docker build` support. Optional; nil means `docker build`
	// falls back to the platform default (buildah on Linux, 501 elsewhere).
	BuildKit *buildkit.Server
	Log      *slog.Logger
}

// Runtime manages container lifecycle for runner environments.
type Runtime struct {
	cfg    Config
	client *client.Client
	pullMu sync.Mutex // serializes image pulls to avoid content store contention
}

// Client returns the underlying containerd client. Used by the in-VM
// debug-exec HTTP server so the Windows host can poke into running
// containers (kindest/node, buildkit) without leaving the VM.
func (r *Runtime) Client() *client.Client {
	return r.client
}

// RunnerEnv represents a running runner environment.
type RunnerEnv struct {
	ID        string
	Netns     string       // network namespace path (Linux only)
	RunnerDir string       // per-job runner copy, cleaned up on destroy
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

	// Clean orphan per-job runner dir copies from `<data-dir>/runners/job-*`.
	// These are ~200MB each and accumulate rapidly when container creation
	// fails after copyDirForJob. No live job needs them at startup — any
	// survivors belong to a previous ephemerd process.
	if r.cfg.RunnerDir != "" {
		runnersParent := filepath.Dir(r.cfg.RunnerDir)
		entries, err := os.ReadDir(runnersParent)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() || !strings.HasPrefix(e.Name(), "job-") {
					continue
				}
				p := filepath.Join(runnersParent, e.Name())
				r.cfg.Log.Info("removing orphan runner dir", "path", p)
				if err := os.RemoveAll(p); err != nil {
					r.cfg.Log.Warn("failed to remove orphan runner dir", "path", p, "error", err)
				}
			}
		}
		// On Windows only: grant the runners parent traverse-only access
		// (no inheritance) so Hyper-V utility VMs can step into per-job
		// subdirectories. Each per-job directory gets its own Modify ACE
		// at Create() time so concurrent jobs stay isolated from each
		// other's runner dirs.
		if err := grantHyperVTraverse(runnersParent); err != nil {
			r.cfg.Log.Warn("failed to grant Hyper-V traverse on runners parent", "path", runnersParent, "error", err)
		}
	}

	// Clean orphan snapshots that no longer have a container pointing to them.
	// This catches snapshots left behind when a container create partially failed.
	ss := "overlayfs"
	if goruntime.GOOS == "windows" {
		ss = "windows"
	}
	snapshotter := r.client.SnapshotService(ss)
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
// Skips tarballs whose images are already present in containerd.
func importTarball(ctx context.Context, c *client.Client, path, snapshotter string, log *slog.Logger) error {
	// Check if the image in this tarball already exists in containerd.
	// Read the tag from the tarball manifest without importing it.
	ref, err := tarballImageRef(path)
	if err == nil && ref != "" {
		if _, getErr := c.GetImage(ctx, ref); getErr == nil {
			log.Info("image already present, skipping import", "name", ref, "path", path)
			return nil
		}
	}

	log.Info("importing image from tarball", "path", path)

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}

	// WithAllPlatforms(true) is required because importTarball is also called
	// cross-platform (e.g. importing a linux/amd64 tarball into a Linux VM's
	// containerd from a Windows host client, where the client's host platform
	// is windows/amd64 and would otherwise filter every manifest out, yielding
	// containerd's "image might be filtered out" error). The tarballs are
	// already platform-filtered at `crane pull --platform=...` time, so trusting
	// the tarball's contents whole is correct.
	imgs, err := c.Import(ctx, f, client.WithAllPlatforms(true))
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

// tarballImageRef reads the image reference (repo tag) from a Docker-format
// tarball's manifest.json. Returns empty string if untagged or unreadable.
func tarballImageRef(path string) (string, error) {
	opener := func() (io.ReadCloser, error) { return os.Open(path) }

	manifest, err := craneTarball.LoadManifest(opener)
	if err != nil {
		return "", err
	}
	if len(manifest) > 0 && len(manifest[0].RepoTags) > 0 {
		return manifest[0].RepoTags[0], nil
	}
	return "", nil
}

// PullImage ensures the runner image is available locally.
// Serialized with a mutex to avoid concurrent pulls contending on
// the content store (which produces noisy lock errors).
func (r *Runtime) PullImage(ctx context.Context, ref string) error {
	return r.withPullLock(func() error {
		return r.pullImageLocked(ctx, ref)
	})
}

// withPullLock runs fn while holding pullMu. Unlock is deferred so a
// panic or error inside fn still releases the lock (no poisoning).
// Extracted for testability — a unit test can swap in a fake fn to
// verify the mutex serializes callers without needing a real
// containerd client.
func (r *Runtime) withPullLock(fn func() error) error {
	r.pullMu.Lock()
	defer r.pullMu.Unlock()
	return fn()
}

func (r *Runtime) pullImageLocked(ctx context.Context, ref string) error {
	ctx = namespaces.WithNamespace(ctx, namespace)

	// Check if another goroutine already pulled/imported it while we waited.
	// Also verify the image is unpacked — the background import may have loaded
	// the content but not yet finished unpacking to the snapshotter.
	snapshotter := "overlayfs"
	if goruntime.GOOS == "windows" {
		snapshotter = "windows"
	}
	if img, err := r.client.GetImage(ctx, ref); err == nil {
		if unpacked, _ := img.IsUnpacked(ctx, snapshotter); unpacked {
			return nil
		}
		// Image exists but isn't unpacked yet — unpack it now.
		r.cfg.Log.Info("image imported but not yet unpacked, unpacking", "ref", ref)
		if err := img.Unpack(ctx, snapshotter); err != nil {
			r.cfg.Log.Warn("unpack failed, will try full pull", "ref", ref, "error", err)
		} else {
			return nil
		}
	}

	// Qualify unqualified Docker Hub refs ("ephpm/ephemerd:tag", "alpine:3")
	// so containerd's resolver doesn't dial the first path segment as a
	// registry host. Refs already containing a registry (host has '.', ':',
	// or is "localhost") pass through unchanged.
	pullRef := qualifyImageRef(ref)
	if pullRef != ref {
		r.cfg.Log.Info("qualifying unqualified image ref for pull",
			"original", ref, "qualified", pullRef)
	}
	r.cfg.Log.Info("pulling image", "ref", pullRef)

	pullOpts := []client.RemoteOpt{
		client.WithPullUnpack,
	}
	if goruntime.GOOS == "windows" {
		pullOpts = append(pullOpts, client.WithPlatform("windows/amd64"))
	}
	pullOpts = append(pullOpts, client.WithPullSnapshotter(snapshotter))
	_, err := r.client.Pull(ctx, pullRef, pullOpts...)
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", pullRef, err)
	}

	// Alias the pulled image under the unqualified name so later
	// GetImage(ref) lookups (config-supplied short refs) succeed.
	if pullRef != ref {
		if img, gerr := r.client.GetImage(ctx, pullRef); gerr == nil {
			imgSvc := r.client.ImageService()
			imgRecord := images.Image{
				Name:   ref,
				Target: img.Target(),
				Labels: map[string]string{"ephemerd.alias-of": pullRef},
			}
			if _, cerr := imgSvc.Create(ctx, imgRecord); cerr != nil {
				if _, uerr := imgSvc.Update(ctx, imgRecord); uerr != nil {
					r.cfg.Log.Warn("aliasing pulled image under unqualified name failed",
						"name", ref, "qualified", pullRef,
						"create_err", cerr, "update_err", uerr)
				}
			}
		}
	}

	r.cfg.Log.Info("image ready", "ref", pullRef)
	return nil
}

// qualifyImageRef ensures a reference carries an explicit registry host.
// Bare names ("alpine") become "docker.io/library/alpine"; namespaced names
// ("ephpm/ephemerd:tag") become "docker.io/ephpm/ephemerd:tag". Refs whose
// first path segment looks like a host (contains '.' or ':', or equals
// "localhost") are returned unchanged.
func qualifyImageRef(ref string) string {
	first := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return ref
	}
	if !strings.Contains(ref, "/") {
		return "docker.io/library/" + ref
	}
	return "docker.io/" + ref
}

// CreateConfig holds parameters for creating a runner environment.
type CreateConfig struct {
	ID    string // unique job identifier (container name, dind socket path)
	Image string // OCI image reference (empty = use default)

	// Provider is the forge provider name (e.g. "github", "gitea") that
	// queued the job. Together with Repo it's used to scope dind's
	// per-repo image cache. Empty disables caching for this job.
	Provider string

	// Repo is the forge-native repo path (e.g. "owner/repo"). Together
	// with Provider it's used to scope dind's per-repo image cache.
	// Empty disables caching for this job.
	Repo string

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
	customImage := image != "" && !isOfficialRunnerImage(image)
	if image == "" {
		if r.cfg.DefaultImage != "" {
			image = r.cfg.DefaultImage
		} else {
			image = defaultImage()
		}
	}

	r.cfg.Log.Info("creating runner environment", "id", id, "image", image, "custom", customImage)

	// Get the image, pulling if needed. Also ensure it's unpacked — the
	// background import goroutine may have loaded the content but not yet
	// finished unpacking to the snapshotter.
	ss := "overlayfs"
	if goruntime.GOOS == "windows" {
		ss = "windows"
	}
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

	// Ensure the image is unpacked. The background import goroutine may have
	// loaded content into the store but not finished unpacking to the snapshotter.
	if unpacked, _ := img.IsUnpacked(ctx, ss); !unpacked {
		r.cfg.Log.Info("image not yet unpacked, unpacking now", "image", image, "snapshotter", ss)
		if err := img.Unpack(ctx, ss); err != nil {
			return nil, fmt.Errorf("unpacking image %s: %w", image, err)
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
	opts = append(opts, rlimitsOpts(r.cfg.Rlimits)...)
	switch {
	case len(cfg.Entrypoint) > 0:
		// Forge mode: custom entrypoint (e.g. act_runner register + daemon).
		opts = append(opts, oci.WithProcessArgs(cfg.Entrypoint...))
	case jitConfig != "" && goruntime.GOOS == "windows":
		// GitHub on Windows: wrap in cmd.exe redirect for log capture.
		// Prepend C:\actions-runner to PATH so the docker.exe we copy into
		// the runner dir (alongside run.cmd) is discoverable by job steps —
		// docker/setup-buildx-action and friends look up `docker` in PATH.
		cmdLine := fmt.Sprintf(`set PATH=C:\actions-runner;%%PATH%% && %s --jitconfig %s > C:\actions-runner\runner.log 2>&1`, entrypoint, jitConfig)
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
	// Per-job runner dir cleanup on error. On success, Destroy() removes it
	// via env.RunnerDir; on failure, the function returns before building
	// the RunnerEnv so we must clean up here or the ~200MB copy orphans on
	// disk (observed: 70 GB accumulated across a few hundred failed jobs).
	createSucceeded := false
	defer func() {
		if !createSucceeded && jobRunnerDir != "" {
			if err := os.RemoveAll(jobRunnerDir); err != nil {
				r.cfg.Log.Warn("failed to remove job runner dir on error", "path", jobRunnerDir, "error", err)
			}
		}
	}()
	if needsRunnerMount {
		jobRunnerDir = filepath.Join(filepath.Dir(r.cfg.RunnerDir), "job-"+id)
		if err := copyDirForJob(r.cfg.RunnerDir, jobRunnerDir); err != nil {
			return nil, fmt.Errorf("copying runner dir for %s: %w", id, err)
		}
		// Hyper-V isolated containers on Windows mount this host directory
		// into the utility VM via a VSMB share. The parent runners dir has
		// already been granted traverse at startup; we grant Modify scoped
		// to this specific job directory so each job's utility VM sees
		// only its own files.
		if err := grantHyperVModify(jobRunnerDir); err != nil {
			return nil, fmt.Errorf("granting Hyper-V access to %s: %w", jobRunnerDir, err)
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
		opts = append(opts, withHostsMount(hostDataDir, containerDataDir, id))
	}

	// Start per-job fake Docker daemon. Exposure to the container differs
	// by platform:
	//   - Linux/macOS: bind-mount the unix socket at /var/run/docker.sock
	//     (standard Docker CLI auto-discovery).
	//   - Windows: DOCKER_HOST=tcp://<hcn-gateway>:<port> env var, because
	//     the OCI Type:"bind" mount isn't supported by runhcs and named pipe
	//     sharing into Hyper-V-isolated containers needs extra HCS plumbing.
	//     docker.exe inside the container picks up DOCKER_HOST and talks TCP.
	var dindServer *dind.Server
	if r.cfg.DindEnabled {
		var err error
		dindServer, err = dind.New(dind.Config{
			JobID:           id,
			Provider:        cfg.Provider,
			Repo:            cfg.Repo,
			DataDir:         r.cfg.DataDir,
			Client:          r.client,
			Network:         r.cfg.Network,
			BuildKit:        r.cfg.BuildKit,
			AllowPrivileged: r.cfg.DindAllowPrivileged,
			Log:             r.cfg.Log,
		})
		if err != nil {
			return nil, fmt.Errorf("creating dind server for %s: %w", id, err)
		}
		if err := dindServer.Start(); err != nil {
			return nil, fmt.Errorf("starting dind server for %s: %w", id, err)
		}
		if goruntime.GOOS == "windows" {
			// oci.WithEnv appends/overrides — safe to call after the initial
			// WithEnv on line 517. The runner's docker CLI (mounted from
			// r.cfg.RunnerDir) sees DOCKER_HOST and talks TCP to our fake
			// daemon on the HCN gateway.
			opts = append(opts, oci.WithEnv([]string{"DOCKER_HOST=" + dindServer.Endpoint()}))
		} else {
			opts = append(opts, withDockerSocket(dindServer.SocketPath()))
		}
	}

	// Add Hyper-V isolation on Windows
	if goruntime.GOOS == "windows" {
		opts = append(opts, withHyperVIsolation())
		opts = append(opts, withWindowsResources(r.cfg.WindowsMemoryBytes, r.cfg.WindowsCPUs))
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

	// Register the runner snapshot + the non-rootfs bind table with the
	// dind server so it can translate sibling `-v` sources from the
	// runner's mount namespace to real host paths. Without this, the GHA
	// runner's `container:` flow asks dind for binds like
	// `/home/runner/_work/_temp` that don't exist on the dind daemon's
	// filesystem — the shim used to silently drop them and the resulting
	// `docker exec sh -e /__w/_temp/<uuid>.sh` failed with "cannot open".
	//
	// The GOOS guard only skips the Windows-native job path (Hyper-V
	// isolated Windows containers with the "windowsfilter" snapshotter —
	// no overlay upperdir to walk, and bind semantics differ enough that
	// translation needs a separate design). Linux jobs on Windows hosts
	// reach this code via the in-VM ephemerd process running as Linux,
	// so they take the registration branch normally.
	if dindServer != nil && goruntime.GOOS != "windows" {
		bindMappings := map[string]string{}
		if dindServer.SocketPath() != "" {
			bindMappings["/var/run/docker.sock"] = dindServer.SocketPath()
		}
		hostDataDir := filepath.Dir(r.cfg.LogDir)
		bindMappings["/etc/hosts"] = filepath.Join(hostDataDir, "hosts", id+".hosts")
		bindMappings["/etc/resolv.conf"] = filepath.Join(hostDataDir, "dns", id+".conf")
		if jobRunnerDir != "" && r.cfg.RunnerMount != "" {
			bindMappings[r.cfg.RunnerMount] = jobRunnerDir
		}
		dindServer.SetRunnerRootfs(snapshotName, bindMappings)
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
		// Tell the dind server which netns to install port-forwarding DNAT
		// rules into. KIND publishes its API server on 127.0.0.1:<random> in
		// the runner's namespace via -p, so we need to install DNAT rules
		// here when a sibling container is created with PortBindings.
		if dindServer != nil {
			dindServer.SetRunnerNetNS(netns)
		}
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

	createSucceeded = true
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

	// Clean up per-job runner directory copy.
	// DEBUG: preserve runner.log for diagnostics — remove this block when done.
	if env.RunnerDir != "" {
		logPath := filepath.Join(env.RunnerDir, "runner.log")
		dirListing, dirErr := os.ReadDir(env.RunnerDir)
		if dirErr != nil {
			r.cfg.Log.Warn("DEBUG failed to list runner dir", "id", env.ID, "dir", env.RunnerDir, "error", dirErr)
		}
		names := []string{}
		for _, d := range dirListing {
			names = append(names, d.Name())
		}
		r.cfg.Log.Info("DEBUG runner dir contents before destroy", "id", env.ID, "dir", env.RunnerDir, "entries", names)
		if logData, readErr := os.ReadFile(logPath); readErr == nil {
			saveDir := r.cfg.LogDir
			if saveDir == "" {
				saveDir = `C:\tmp`
			}
			if err := os.MkdirAll(saveDir, 0o755); err != nil {
				r.cfg.Log.Warn("DEBUG failed to create log save dir", "id", env.ID, "dir", saveDir, "error", err)
			}
			savePath := filepath.Join(saveDir, env.ID+"-runner.log")
			if werr := os.WriteFile(savePath, logData, 0o644); werr != nil {
				r.cfg.Log.Warn("DEBUG failed to save runner.log", "id", env.ID, "error", werr)
			} else {
				r.cfg.Log.Info("DEBUG preserved runner.log", "id", env.ID, "path", savePath, "bytes", len(logData))
			}
		} else {
			r.cfg.Log.Warn("DEBUG runner.log missing", "id", env.ID, "path", logPath, "error", readErr)
		}
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

// withHostsMount writes a per-container /etc/hosts file and bind-mounts
// it into the container. Docker does this by default; without it, the
// image's /etc/hosts (often empty in actions-runner-style images) leaves
// "localhost" without a files-side entry, so Go programs that call
// net.Listen("tcp", "localhost:10350") fall through to DNS and fail with
// "lookup localhost on 1.1.1.1:53: no such host" — exactly what tilt ci
// hits inside our self-hosted runner.
func withHostsMount(hostDir, containerDir, containerID string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		content := "127.0.0.1\tlocalhost\n" +
			"::1\tlocalhost ip6-localhost ip6-loopback\n" +
			"fe00::0\tip6-localnet\n" +
			"ff00::0\tip6-mcastprefix\n" +
			"ff02::1\tip6-allnodes\n" +
			"ff02::2\tip6-allrouters\n"

		dir := filepath.Join(hostDir, "hosts")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating hosts dir: %w", err)
		}
		hostFile := filepath.Join(dir, containerID+".hosts")
		if err := os.WriteFile(hostFile, []byte(content), 0o644); err != nil {
			return fmt.Errorf("writing hosts: %w", err)
		}

		src := filepath.Join(containerDir, "hosts", containerID+".hosts")
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: "/etc/hosts",
			Type:        "bind",
			Source:      src,
			Options:     []string{"rbind", "ro"},
		})
		return nil
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
// On Windows, uses a native Go walk+copy — xcopy returned exit 4 (init
// error) intermittently when invoked under the SYSTEM service account,
// even though manual runs from the same user worked. Going native avoids
// the external-command dependency and surfaces real I/O errors.
func copyDirForJob(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if goruntime.GOOS == "windows" {
		return copyDirNative(src, dst)
	}
	return exec.Command("cp", "-al", src, dst).Run()
}

// copyDirNative recursively copies src to dst using only the standard
// library. Symlinks are resolved (the GitHub Actions runner directory
// doesn't contain any symlinks on Windows).
func copyDirNative(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode())
		case info.Mode()&os.ModeSymlink != 0:
			realPath, rerr := filepath.EvalSymlinks(path)
			if rerr != nil {
				return fmt.Errorf("resolving symlink %s: %w", path, rerr)
			}
			ri, rerr := os.Stat(realPath)
			if rerr != nil {
				return fmt.Errorf("stat symlink target %s: %w", realPath, rerr)
			}
			if ri.IsDir() {
				return copyDirNative(realPath, target)
			}
			return copyFileNative(realPath, target, ri.Mode())
		default:
			return copyFileNative(path, target, info.Mode())
		}
	})
}

func copyFileNative(src, dst string, mode os.FileMode) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := in.Close(); cerr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close %s: %w", src, cerr))
		}
	}()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		if cerr := out.Close(); cerr != nil {
			return errors.Join(fmt.Errorf("copying %s: %w", src, err), fmt.Errorf("close %s: %w", dst, cerr))
		}
		return err
	}
	return out.Close()
}

// isOfficialRunnerImage reports whether image is a stock GitHub Actions
// runner image — those put run.sh under /home/runner, while every other
// image gets our embedded runner bind-mounted at /actions-runner. The
// scheduler resolves the default image on the host before dispatching to
// the Linux VM, so by the time runtime.Create sees the ref the "image was
// not specified by the caller" signal is already lost; we recover it by
// matching the well-known official refs here.
func isOfficialRunnerImage(image string) bool {
	for _, prefix := range []string{
		"ghcr.io/actions/actions-runner:",
		"ghcr.io/actions/actions-runner@",
		"ghcr.io/actions/runner-images-runner:",
		// ephemerd's runner-ci-* images are FROM ghcr.io/actions/actions-runner
		// with extra build deps baked in, so the runner is at the same path
		// (/home/runner/run.sh). Without this entry the runtime treats them
		// as foreign images and bind-mounts /actions-runner over the rootfs,
		// then runs /actions-runner/run.sh — which the image doesn't have,
		// so the entrypoint exits 127 ("command not found").
		"ephpm/ephemerd:runner-ci-linux-",
		"docker.io/ephpm/ephemerd:runner-ci-linux-",
	} {
		if strings.HasPrefix(image, prefix) {
			return true
		}
	}
	return false
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

// withWindowsResources sets memory and CPU limits on a Windows container.
// Without limits, Hyper-V isolated containers default to ~1 GB RAM, which
// is too small for MSVC + parallel cl.exe builds. Either argument being 0
// leaves the corresponding OCI spec field unset (HCS default applies).
func withWindowsResources(memoryBytes, cpus uint64) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if memoryBytes == 0 && cpus == 0 {
			return nil
		}
		if s.Windows == nil {
			s.Windows = &ocispec.Windows{}
		}
		if s.Windows.Resources == nil {
			s.Windows.Resources = &ocispec.WindowsResources{}
		}
		if memoryBytes > 0 {
			limit := memoryBytes
			s.Windows.Resources.Memory = &ocispec.WindowsMemoryResources{Limit: &limit}
		}
		if cpus > 0 {
			count := cpus
			s.Windows.Resources.CPU = &ocispec.WindowsCPUResources{Count: &count}
		}
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
