package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
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
	"github.com/ephpm/ephemerd/pkg/imagegc"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/proxies"
	"github.com/ephpm/ephemerd/pkg/registrymirror"
	craneTarball "github.com/google/go-containerregistry/pkg/v1/tarball"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// Namespace is the containerd namespace every runner container, and every
// image the runtime pulls, lives in. Exported so the image collector can be
// pointed at it without duplicating the string.
const Namespace = "ephemerd"

const (
	namespace         = Namespace
	defaultImageLinux = "ghcr.io/actions/actions-runner:latest"
)

// containerCapabilities is the minimum set of Linux capabilities for CI jobs.
// Covers apt-get install, sudo, adduser, and service management.
//
// A real nested dockerd is deliberately out of reach with this set — that is
// what the missing CAP_SYS_ADMIN/CAP_NET_ADMIN buy. Jobs still get a working
// `docker`: pkg/dind serves a per-job Docker API from the host and creates
// sibling containers on the host containerd, so no capability inside the job
// container is involved. (An older revision of this comment said to use
// Kaniko or Buildah instead; that predates pkg/dind.)
//
// CAP_MKNOD is deliberately absent. It lets a process create device nodes,
// which is the first step of the classic "mknod a block device for the host
// disk and read it raw" escape. containerd's default spec already installs a
// deny-all device cgroup, so a node created this way would not be usable
// anyway — dropping the capability just removes the reliance on that single
// layer. No package in our supported runner images needs mknod at install time
// (containers get their /dev from the runtime); a handful of packages
// elsewhere in the distro archive do, so if one is ever found to need it, add
// it back for that pool only rather than fleet wide.
var containerCapabilities = []string{
	"CAP_CHOWN",            // dpkg chown on installed files
	"CAP_DAC_OVERRIDE",     // write to dirs owned by other users
	"CAP_FOWNER",           // chmod/utimes on files not owned by process
	"CAP_FSETID",           // preserve SUID/SGID bits (sudo, passwd)
	"CAP_KILL",             // signal processes (postinst service restarts)
	"CAP_SETGID",           // adduser/addgroup in maintainer scripts
	"CAP_SETUID",           // setuid in maintainer scripts
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
	// RegistryMirror routes image pulls through a LAN pull-through cache.
	// Nil (the zero value) means no mirror: PullImage builds exactly the
	// containerd pull call it built before the feature existed. Forwarded
	// to each per-job dind.Server so the hot dind pull path is covered too.
	RegistryMirror *registrymirror.Mirror
	CacheProxyEnv  []string // extra env vars from cache proxies (e.g., GOPROXY=...)
	// CacheProxyMounts are read-only bind mounts requested by cache proxies
	// for toolchains that cannot be redirected with an env var. The Cargo
	// proxy uses this to place a generated .cargo/config.toml at the
	// container's filesystem root, where Cargo's ancestor-directory config
	// search finds it for any workspace path.
	CacheProxyMounts []proxies.Mount
	// Rlimits sets POSIX resource limits on each runner container's OCI
	// process. Zero values fall back to the containerd default (1024).
	// Applies on Linux only; ignored on Windows (HCS uses a different model).
	Rlimits config.RuntimeRlimits
	// AllowNewPrivileges permits privilege escalation via execve in the
	// runner container (NoNewPrivileges=false), which is what makes
	// `sudo` work. See config.RuntimeConfig.AllowNewPrivileges.
	//
	// NOTE: this is a resolved value — callers pass
	// cfg.Runtime.ResolvedAllowNewPrivileges(), whose default is true.
	// The zero value here is false, i.e. the hardened setting, so a
	// construction site that forgets this field breaks `sudo` rather
	// than silently loosening the sandbox.
	AllowNewPrivileges bool
	// LinuxRuntime is the containerd runtime handler for Linux job
	// containers — "io.containerd.runc.v2" (default) or
	// "io.containerd.kata.v2" for VM-isolated jobs. Empty means runc, so
	// a construction site that forgets this field keeps today's behavior
	// rather than failing to create containers. Ignored on Windows, which
	// always uses io.containerd.runhcs.v1.
	//
	// Callers pass config.LinuxRunnerToml.ContainerdRuntime().
	LinuxRuntime string
	Network      *networking.Manager
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
	// OnTaskStarted is invoked synchronously by Create after the container
	// task has successfully Start()ed. Nil means no hook. Used to wire
	// per-container resource samplers into the metrics endpoint; see
	// docs/arch/container-metrics.md.
	OnTaskStarted func(env *RunnerEnv)
	// OnTaskDestroy is invoked synchronously by Destroy before the
	// container is torn down. Symmetric with OnTaskStarted.
	OnTaskDestroy func(env *RunnerEnv)
	// ImageGC evicts LRU container images when the node is under disk
	// pressure. Nil disables it. The runtime consults it before pulling
	// an image and before creating a runner environment — a periodic
	// timer alone loses the race a single job can win by pulling a
	// multi-gigabyte toolchain image between ticks.
	ImageGC *imagegc.Collector
	Log     *slog.Logger
}

// resolveRuntimeName picks the containerd runtime handler for a job
// container.
//
// The runtime is always named explicitly: containerd 2.2 may otherwise
// default to the experimental io.containerd.nerdbox.v1 runtime, whose shim
// binary isn't in our embed.
//
// On Linux the handler comes from [runner.linux] runtime — runc by
// default, io.containerd.kata.v2 when the operator opted into VM-isolated
// jobs. Windows always uses the host runhcs shim; the Linux knob does not
// apply there. An empty linuxRuntime means runc, so a construction site
// that forgets the field keeps today's behavior.
func resolveRuntimeName(linuxRuntime, goos string) string {
	if goos == "windows" {
		return "io.containerd.runhcs.v1"
	}
	if linuxRuntime == "" {
		return "io.containerd.runc.v2"
	}
	return linuxRuntime
}

// kataRuntimeName is the containerd runtime handler for Kata Containers —
// the one Linux handler that puts the job container in its own VM, with its
// own kernel.
const kataRuntimeName = "io.containerd.kata.v2"

// resolveDindTransport picks how the per-job Docker API is handed to the job
// container.
//
// The deciding question is whether the container shares the host's kernel.
// When it does (runc), a bind-mounted unix socket is the cheapest and most
// Docker-native answer, and stays the default. When it does not — a Kata
// guest on Linux, a Hyper-V-isolated container on Windows — the bind carries
// the socket inode across the VM boundary but not the listening endpoint
// behind it, so every connect(2) in the guest returns ECONNREFUSED. Those get
// TCP on the bridge gateway, which the guest reaches over IP like any other
// service. It is the same failure and the same fix on both platforms; only
// the trigger differs, and on Linux the trigger is a runtime choice rather
// than the build target, which is why this cannot be a build tag.
func resolveDindTransport(linuxRuntime, goos string) dind.Transport {
	if goos == "windows" {
		return dind.TransportTCP
	}
	if resolveRuntimeName(linuxRuntime, goos) == kataRuntimeName {
		return dind.TransportTCP
	}
	return dind.TransportUnixSocket
}

// Runtime manages container lifecycle for runner environments.
type Runtime struct {
	cfg    Config
	client *client.Client
	pullMu sync.Mutex // serializes image pulls to avoid content store contention

	// provisioning holds the IDs of jobs whose on-disk state (runner-dir copy,
	// job workdir, snapshot) exists but whose container is not yet registered
	// in containerd — i.e. jobs somewhere between the copyDirForJob and
	// NewContainer calls in Create. The orphan sweep decides "orphan" by the
	// absence of a containerd container, so without this a sweep that fires
	// during the provisioning window (a Windows image pull can take minutes)
	// deletes a live job's runner dir out from under it, corrupting it into a
	// self-update loop (observed on metal 2026-08-14). SweepOrphans unions
	// these IDs into its keep set.
	provMu       sync.Mutex
	provisioning map[string]struct{}
}

// beginProvisioning marks id as in-flight so the orphan sweep will not reclaim
// its on-disk state before its container exists. The returned func clears it
// and must be deferred by the caller.
func (r *Runtime) beginProvisioning(id string) func() {
	r.provMu.Lock()
	if r.provisioning == nil {
		r.provisioning = make(map[string]struct{})
	}
	r.provisioning[id] = struct{}{}
	r.provMu.Unlock()
	return func() {
		r.provMu.Lock()
		delete(r.provisioning, id)
		r.provMu.Unlock()
	}
}

// addProvisioning inserts the in-flight provisioning IDs into keep so a sweep
// preserves their on-disk state.
func (r *Runtime) addProvisioning(keep map[string]struct{}) {
	r.provMu.Lock()
	defer r.provMu.Unlock()
	for id := range r.provisioning {
		keep[id] = struct{}{}
	}
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
	Provider  string       // forge provider that queued the job (e.g. "github")
	Repo      string       // forge-native repo path (e.g. "owner/repo")
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

// SetTaskHooks installs (or replaces) the OnTaskStarted / OnTaskDestroy
// callbacks. Used by main.go to wire metrics-sampler registration after
// both the runtime and the dispatch server have been constructed —
// constructor-order makes a plain Config-time hook awkward because the
// dispatch server depends on the runtime.
func (r *Runtime) SetTaskHooks(onStarted, onDestroy func(*RunnerEnv)) {
	r.cfg.OnTaskStarted = onStarted
	r.cfg.OnTaskDestroy = onDestroy
}

// CleanOrphans removes any leftover containers and snapshots from a previous
// ephemerd run. This should be called on startup before the scheduler starts
// accepting jobs.
//
// STARTUP ONLY. It kills and deletes EVERY container in the runtime
// namespace on the assumption that nothing legitimate is running yet.
// Calling it while jobs are in flight would tear those jobs down. The
// periodic equivalent is SweepOrphans, which never touches containers.
func (r *Runtime) CleanOrphans(ctx context.Context) error {
	ctx = namespaces.WithNamespace(ctx, namespace)

	// Unmount dind bind-staging mounts left by a previous process. This runs
	// FIRST, before any container or snapshot deletion: each leaked staging
	// mount holds a reference to the runner rootfs it was bound from, so
	// while one is present containerd cannot delete that container's
	// snapshot and the sweep below silently fails to reclaim the space.
	// A hard kill (SIGKILL, panic, node reset) is the case that produces
	// them — every graceful path unmounts as it goes.
	dind.SweepStagedBinds(r.cfg.DataDir, r.cfg.Log)

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

	// On Windows only: grant the runners parent traverse-only access (no
	// inheritance) so Hyper-V utility VMs can step into per-job
	// subdirectories. Each per-job directory gets its own Modify ACE at
	// Create() time so concurrent jobs stay isolated from each other's
	// runner dirs. Startup-only — the ACE does not need re-applying.
	if r.cfg.RunnerDir != "" {
		runnersParent := filepath.Dir(r.cfg.RunnerDir)
		if err := grantHyperVTraverse(runnersParent); err != nil {
			r.cfg.Log.Warn("failed to grant Hyper-V traverse on runners parent", "path", runnersParent, "error", err)
		}
	}

	// Every container is gone now, so nothing on disk is live: sweep with
	// an empty keep set.
	return r.sweepOrphanState(ctx, nil)
}

// SweepOrphans removes per-job state that no existing container owns:
// leftover runner-dir copies under <data-dir>/runners/job-*, per-job
// workdirs under <data-dir>/jobs/*, and writable container snapshots.
//
// Unlike CleanOrphans it never touches containers, tasks or networking, so
// it is safe to run on a timer while jobs are in flight. That matters
// because the leaks it cleans up are produced by crashes and partially
// failed creates, which a startup-only sweep leaves to accumulate for the
// entire uptime of a long-lived daemon.
//
// Cost is one container list plus one readdir per directory plus a
// snapshotter walk — cheap enough for a ~60s cadence.
func (r *Runtime) SweepOrphans(ctx context.Context) error {
	ctx = namespaces.WithNamespace(ctx, namespace)

	containers, err := r.client.Containers(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	live := make(map[string]struct{}, len(containers))
	for _, c := range containers {
		live[c.ID()] = struct{}{}
	}
	// Jobs mid-provision have on-disk state but no container yet: keep them.
	r.addProvisioning(live)
	return r.sweepOrphanState(ctx, live)
}

// sweepOrphanState is the shared body of CleanOrphans and SweepOrphans.
// live is the set of container IDs whose on-disk state must be preserved;
// nil means "nothing is live".
//
// ctx must already carry the runtime namespace.
func (r *Runtime) sweepOrphanState(ctx context.Context, live map[string]struct{}) error {
	// Clean orphan per-job runner dir copies from `<data-dir>/runners/job-*`.
	// These are ~200MB each and accumulate rapidly when container creation
	// fails after copyDirForJob (observed: 70 GB across a few hundred
	// failed jobs).
	if r.cfg.RunnerDir != "" {
		runnersParent := filepath.Dir(r.cfg.RunnerDir)
		entries, err := os.ReadDir(runnersParent)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() || !strings.HasPrefix(e.Name(), "job-") {
					continue
				}
				// Create() names each copy "job-<container id>".
				if _, ok := live[strings.TrimPrefix(e.Name(), "job-")]; ok {
					continue
				}
				p := filepath.Join(runnersParent, e.Name())
				r.cfg.Log.Info("removing orphan runner dir", "path", p)
				if err := os.RemoveAll(p); err != nil {
					r.cfg.Log.Warn("failed to remove orphan runner dir", "path", p, "error", err)
				}
			}
		}
	}

	// Sweep leftover per-job workdirs from <data>/jobs/. Destroy removes each
	// job's dir on completion, but a crash / SIGKILL of a previous ephemerd
	// process (or a job dir left by a build that predates the per-job removal)
	// skips that path.
	CleanOrphanJobDirs(r.cfg.DataDir, live, r.cfg.Log)

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

	return snapshotter.Walk(ctx, func(snapCtx context.Context, info snapshots.Info) error {
		// Only clean ephemerd snapshots (they all end with -snapshot)
		if !strings.HasSuffix(info.Name, "-snapshot") {
			return nil
		}
		// Create() names each snapshot "<container id>-snapshot".
		if _, ok := live[strings.TrimSuffix(info.Name, "-snapshot")]; ok {
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
		// Stamp the LRU key so an imported image starts its life "just
		// used" rather than looking never-accessed to the image GC.
		imagegc.Touch(ctx, c, namespace, img.Name, log)
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
			r.touchImage(ctx, ref)
			return nil
		}
		// Image exists but isn't unpacked yet — unpack it now.
		r.cfg.Log.Info("image imported but not yet unpacked, unpacking", "ref", ref)
		if err := img.Unpack(ctx, snapshotter); err != nil {
			r.cfg.Log.Warn("unpack failed, will try full pull", "ref", ref, "error", err)
		} else {
			r.touchImage(ctx, ref)
			return nil
		}
	}

	// About to fetch (and extract) potentially gigabytes. Reclaim first if
	// the disk is already over a watermark — the periodic sweep runs every
	// ~60s and a single toolchain pull can cross the line inside one tick.
	r.cfg.ImageGC.EnsureHeadroom(ctx)

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
	// Route through the LAN pull-through cache when one is configured.
	// Appends nothing when it isn't, so the unconfigured pull is unchanged.
	// The mirror resolver keeps the origin registry behind the cache, so a
	// dead cache costs one failed request rather than a failed job.
	r.cfg.RegistryMirror.LogPull(pullRef)
	pullOpts = append(pullOpts, r.cfg.RegistryMirror.PullOpts(nil)...)
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

	r.touchImage(ctx, ref)
	if pullRef != ref {
		r.touchImage(ctx, pullRef)
	}

	r.cfg.Log.Info("image ready", "ref", pullRef)
	return nil
}

// touchImage refreshes the image's ephemerd.io/last-accessed label in the
// runtime namespace. That label is the LRU key the image GC evicts by; the
// runtime namespace had nothing writing it before, so every record looked
// equally cold and the fallback (UpdatedAt) was the only ordering signal.
//
// ctx must already carry the runtime namespace. Best-effort by design — a
// failed label write is logged at debug and never fails a job.
func (r *Runtime) touchImage(ctx context.Context, ref string) {
	imagegc.Touch(ctx, r.client, namespace, ref, r.cfg.Log)
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

	// Protect this job's on-disk state (runner-dir copy, job workdir, snapshot)
	// from the orphan sweep for the whole provisioning window — from here until
	// Create returns. Until NewContainer runs there is no containerd container
	// for the sweep to key off, so without this a sweep firing mid-provision
	// (an image pull can take minutes) would delete a live job's runner dir.
	doneProvisioning := r.beginProvisioning(id)
	defer doneProvisioning()

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

	// A new runner environment means a fresh writable snapshot plus
	// whatever the job writes into it. Reclaim before we commit to that if
	// the node is already over a watermark.
	r.cfg.ImageGC.EnsureHeadroom(ctx)

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

	// The image is in use as of now — refresh the LRU key so a job's image
	// cannot be evicted as "cold" simply because it was pulled days ago and
	// has been in steady use since.
	r.touchImage(ctx, image)

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
		// Restrict capabilities to the minimum needed for CI jobs.
		// This covers apt-get install, adduser, sudo, and service management.
		// A nested dockerd is out of reach here (no CAP_SYS_ADMIN/CAP_NET_ADMIN);
		// `docker` in the job is served by pkg/dind from the host instead —
		// see containerCapabilities.
		oci.WithCapabilities(containerCapabilities),
	}
	// Point the runner's tool cache at a path inside the image. Applied after
	// WithImageConfig/WithEnv so the image and the job keep the last word —
	// see withDefaultEnv and the WindowsToolCache comment for why the runner's
	// own default (<runner root>\_work\_tool) is unusable here: that path is
	// the per-job host directory we map in, so nothing baked into the image
	// survives there and every setup-* action re-extracts its toolchain over
	// VSMB. Windows only: on Linux the runner root lives in the image already
	// and overlayfs small-file writes are not the bottleneck.
	if goruntime.GOOS == "windows" {
		opts = append(opts, withDefaultEnv("RUNNER_TOOL_CACHE", WindowsToolCache))
	}
	// Cache-proxy config mounts (e.g. the Cargo source-replacement config).
	// Read-only: a job must never be able to rewrite what the next job on
	// this host will read.
	if len(r.cfg.CacheProxyMounts) > 0 {
		opts = append(opts, withCacheProxyMounts(r.cfg.CacheProxyMounts))
	}
	opts = append(opts, seccompOpts()...)
	// AppArmor is an additional, independent layer over what the default spec
	// above already does (read-only /proc/sys and /sys, masked /proc paths,
	// deny-all device cgroup) and over seccomp. It constrains file operations,
	// which syscall filtering does not distinguish between. No-op where the
	// host has no usable AppArmor — see apparmorOpts for the fail-open
	// rationale and the log line that reports it.
	opts = append(opts, apparmorOpts(r.cfg.Log)...)
	// Privilege escalation via execve — allowed by default so `sudo
	// apt-get install` works. See config.RuntimeConfig.AllowNewPrivileges.
	opts = append(opts, newPrivilegesOpts(r.cfg.AllowNewPrivileges)...)
	opts = append(opts, rlimitsOpts(r.cfg.Rlimits)...)
	switch {
	case len(cfg.Entrypoint) > 0:
		// Forge mode: custom entrypoint (e.g. act_runner register + daemon).
		opts = append(opts, oci.WithProcessArgs(cfg.Entrypoint...))
	case jitConfig != "" && goruntime.GOOS == "windows":
		// GitHub on Windows: wrap in cmd.exe redirect for log capture.
		//
		// C:\actions-runner (RunnerMount) is prepended to PATH. Why is not
		// recorded anywhere we can find, and the reason the comment used to
		// give was false (see below) — so treat it as load-bearing-unknown
		// rather than guessing a new rationale on top of a wrong one. It is
		// harmless and jobs may depend on it; establish what depends on it
		// before removing it.
		//
		// It does NOT put a docker CLI there, and no code in this repo ever
		// has: a previous version of this comment claimed "the docker.exe we
		// copy into the runner dir", which sent one investigation looking for
		// a copy that does not exist. On Windows the docker CLI comes from the
		// runner IMAGE (images/runner-ci-windows installs it at C:\go\bin) —
		// so a Windows job running on a stock image, including the servercore
		// default in image_windows.go and any image a workflow names in
		// `container:`, has no `docker` on PATH and fails with
		// "docker: command not found" the moment anything looks for one.
		// Adding the binary would not by itself make `container:` work; see
		// checkWindowsSiblingGate in pkg/dind for the half that is missing.
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
	// runnerWritableDir is the host path holding this job's WRITABLE copy of
	// the runner tree (the overlay upperdir on Linux, the byte copy on
	// Windows). dind resolves sibling `docker -v` sources under the runner
	// mount against it. Empty when no runner mount is installed.
	var runnerWritableDir string
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
		writableDir, runnerOpt, err := prepareJobRunnerTree(r.cfg.RunnerDir, jobRunnerDir, r.cfg.RunnerMount)
		if err != nil {
			return nil, fmt.Errorf("preparing runner dir for %s: %w", id, err)
		}
		runnerWritableDir = writableDir
		// Hyper-V isolated containers on Windows mount this host directory
		// into the utility VM via a VSMB share. The parent runners dir has
		// already been granted traverse at startup; we grant Modify scoped
		// to this specific job directory so each job's utility VM sees
		// only its own files. No-op on Linux.
		if err := grantHyperVModify(jobRunnerDir); err != nil {
			return nil, fmt.Errorf("granting Hyper-V access to %s: %w", jobRunnerDir, err)
		}
		opts = append(opts, runnerOpt)
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

	// Start per-job fake Docker daemon. How it reaches the container depends
	// on whether the container shares the host kernel — see
	// resolveDindTransport:
	//   - Kernel-sharing container (runc on Linux/macOS): bind-mount the unix
	//     socket at /var/run/docker.sock, standard Docker CLI auto-discovery.
	//   - VM-isolated container (Kata on Linux, Hyper-V on Windows):
	//     DOCKER_HOST=tcp://<gateway>:<port>, because a bind-mounted socket
	//     inode has no endpoint behind it once it crosses into a guest with
	//     its own kernel. The docker CLI inside the container picks up
	//     DOCKER_HOST and talks TCP.
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
			RegistryMirror:  r.cfg.RegistryMirror,
			Transport:       resolveDindTransport(r.cfg.LinuxRuntime, goruntime.GOOS),
			Log:             r.cfg.Log,
		})
		if err != nil {
			return nil, fmt.Errorf("creating dind server for %s: %w", id, err)
		}
		if err := dindServer.Start(); err != nil {
			return nil, fmt.Errorf("starting dind server for %s: %w", id, err)
		}
		// An empty SocketPath is the TCP transport: there is nothing to
		// mount, and the endpoint goes in as an env var instead. oci.WithEnv
		// appends/overrides, so it is safe to call after the initial WithEnv
		// above.
		if sock := dindServer.SocketPath(); sock != "" {
			opts = append(opts, withDockerSocket(sock))
		} else {
			opts = append(opts, oci.WithEnv([]string{"DOCKER_HOST=" + dindServer.Endpoint()}))
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
			// Setup applies the per-endpoint egress ACLs (RFC1918 + link-local
			// block) and returns an error if it could not — there is no global
			// firewall backstop on Windows. Fail CLOSED: abort the job rather
			// than start a container with an unfirewalled (or absent) endpoint.
			if dindServer != nil {
				dindServer.Stop()
			}
			return nil, fmt.Errorf("setting up Windows network endpoint for %s: %w", id, err)
		}
		if result != nil {
			windowsEndpointID = result.EndpointID
			windowsNetNS = result.NetNS
			opts = append(opts, withWindowsNetwork(windowsNetNS, windowsEndpointID))

			// The container's address exists only now — Setup either allocates
			// it out of the L2Bridge pool or reads back what HNS assigned on the
			// NAT network — so this is the first moment the dind host-port allow
			// can be scoped to the one container entitled to use it. It has to
			// be scoped: the dind Docker API is unauthenticated, so an allow
			// covering the whole subnet would let any other job's container
			// drive this job's daemon. Still ahead of container creation below,
			// so the container never runs without its allow.
			//
			// Fail CLOSED on error rather than falling back to a wider allow:
			// losing docker in this job beats losing isolation in all of them.
			if dindServer != nil {
				if err := dindServer.SetRunnerIP(result.IP); err != nil {
					dindServer.Stop()
					if tearErr := r.cfg.Network.Teardown(ctx, id, windowsNetNS); tearErr != nil {
						r.cfg.Log.Debug("endpoint cleanup after failed dind host-port open", "error", tearErr)
					}
					return nil, fmt.Errorf("authorizing dind access for %s: %w", id, err)
				}
			}
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

	runtimeName := resolveRuntimeName(r.cfg.LinuxRuntime, goruntime.GOOS)
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
		// Tell the dind server which netns to install port-forwarding DNAT
		// rules into. KIND publishes its API server on 127.0.0.1:<random> in
		// the runner's namespace via -p, so we need to install DNAT rules
		// here when a sibling container is created with PortBindings.
		if dindServer != nil {
			dindServer.SetRunnerNetNS(netns)
		}
		setupResult, err := r.cfg.Network.Setup(ctx, id, netns)
		if err != nil {
			stopDind()
			if _, delErr := task.Delete(ctx, client.WithProcessKill); delErr != nil {
				r.cfg.Log.Debug("task cleanup after failed network setup", "error", delErr)
			}
			if delErr := container.Delete(ctx, client.WithSnapshotCleanup); delErr != nil {
				r.cfg.Log.Debug("container cleanup after failed network setup", "error", delErr)
			}
			return nil, fmt.Errorf("setting up network for %s: %w", id, err)
		}

		// Register the runner container as a member of its own job so it can
		// reach — and be reached by — this job's dind sibling and `services:`
		// containers, while the bridge denies it any OTHER job's containers.
		// cniID == jobID == id for the runner; siblings join later under the
		// same jobID from the dind server. Best-effort: a failure here only
		// costs within-job container-to-container reach, never opens a cross-job
		// path, so it must not abort the job.
		if setupResult != nil {
			if jErr := r.cfg.Network.JoinJobNetwork(id, id, setupResult.IP); jErr != nil {
				r.cfg.Log.Warn("failed to register runner in job network (intra-job container reach may be limited)", "id", id, "error", jErr)
			}
		}

		// The container's address exists only now — CNI is what allocates it
		// — so this is the first moment the dind TCP port can be scoped to
		// the one container entitled to use it. It has to be scoped: the dind
		// Docker API is unauthenticated and every container on the bridge can
		// address the gateway, so an unscoped port would let any concurrent
		// job drive this job's daemon. A no-op on the unix-socket transport,
		// which binds no port.
		//
		// Still ahead of task.Start below, so the container never runs
		// without its allow in place. Fail CLOSED rather than starting with a
		// wider (or no) scope: losing docker in this job beats losing
		// isolation in all of them.
		if dindServer != nil && setupResult != nil {
			if err := dindServer.SetRunnerIP(setupResult.IP); err != nil {
				stopDind()
				r.cfg.Network.LeaveJobNetwork(id)
				if tearErr := r.cfg.Network.Teardown(ctx, id, netns); tearErr != nil {
					r.cfg.Log.Debug("network teardown after failed dind port scope", "error", tearErr)
				}
				if _, delErr := task.Delete(ctx, client.WithProcessKill); delErr != nil {
					r.cfg.Log.Debug("task cleanup after failed dind port scope", "error", delErr)
				}
				if delErr := container.Delete(ctx, client.WithSnapshotCleanup); delErr != nil {
					r.cfg.Log.Debug("container cleanup after failed dind port scope", "error", delErr)
				}
				return nil, fmt.Errorf("authorizing dind access for %s: %w", id, err)
			}
		}
	}

	if err := task.Start(ctx); err != nil {
		stopDind()
		teardownNetNS := netns
		if windowsNetNS != "" {
			teardownNetNS = windowsNetNS
		}
		if r.cfg.Network != nil && (netns != "" || windowsEndpointID != "") {
			r.cfg.Network.LeaveJobNetwork(id)
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

	// Register the runner's rootfs mount path + non-rootfs bind table
	// with the dind server so it can translate sibling `-v` sources
	// from the runner's mount namespace to real host paths.
	//
	// The bundle rootfs is the merged overlay path runc mounts at
	// `<state-dir>/io.containerd.runtime.v2.task/<ns>/<task-id>/rootfs`
	// — a regular host directory that contains the runner's full
	// filesystem view (image lowerdirs + writable upperdir). Binds
	// from it succeed at mount(2) and expose every layer's content.
	//
	// We construct it deterministically from r.cfg.DataDir rather than
	// readlinking /proc/<pid>/root, which returns "/" on this kernel
	// for reasons we haven't fully traced (no separate mount namespace
	// visible from outside, or chroot-not-pivot_root, or shim-relative
	// addressing) — see the earlier rounds of this fix in PR history.
	if dindServer != nil && goruntime.GOOS != "windows" {
		runnerRootfsPath := filepath.Join(r.cfg.DataDir, "containerd", "state",
			"io.containerd.runtime.v2.task", namespace, id, "rootfs")
		if _, statErr := os.Stat(runnerRootfsPath); statErr != nil {
			r.cfg.Log.Warn("computed runner rootfs path does not exist; sibling binds will fall back to snapshot layer walk",
				"path", runnerRootfsPath, "error", statErr)
			runnerRootfsPath = ""
		}
		bindMappings := map[string]string{}
		if dindServer.SocketPath() != "" {
			bindMappings["/var/run/docker.sock"] = dindServer.SocketPath()
		}
		hostDataDir := filepath.Dir(r.cfg.LogDir)
		bindMappings["/etc/hosts"] = filepath.Join(hostDataDir, "hosts", id+".hosts")
		bindMappings["/etc/resolv.conf"] = filepath.Join(hostDataDir, "dns", id+".conf")
		if runnerWritableDir != "" && r.cfg.RunnerMount != "" {
			// Map the runner mount to the job's WRITABLE layer, not the shared
			// tree. On Linux that is the overlay upperdir: the runner creates
			// _work/ (job checkouts, _temp, _actions) under the mount, all of
			// which is copied up there, so a sibling `docker -v
			// /actions-runner/_work/...` resolves to the same bytes the runner
			// sees. dind and the runner share this one upperdir, so a lazy dir
			// dind creates for a bind source also appears live in the runner.
			//
			// One consequence to be precise about: a runner base-tree file that
			// lives ONLY in the shared lowerdir and was never copied up is not
			// present under this upperdir. A sibling `docker -v
			// /actions-runner/<lower-only-file>` therefore does NOT fail to
			// resolve — it hits translateBindSource's Docker-compatible
			// auto-mkdir-on-missing-source path (pinBindSource autoCreate) and
			// the sibling gets a freshly created EMPTY directory, not the base
			// file's bytes. Real jobs only bind `_work/`, which lives in the
			// upper, so this is a benign known limitation rather than a
			// resolution error; a follow-up could add the lowerdir to the dind
			// resolver's search path if a workflow ever needs it.
			bindMappings[r.cfg.RunnerMount] = runnerWritableDir
		}
		dindServer.SetRunnerRootfs(snapshotName, runnerRootfsPath, bindMappings)
	}

	r.cfg.Log.Info("runner environment started", "id", id)

	// On Windows, use the HCN namespace ID for teardown
	envNetns := netns
	if windowsNetNS != "" {
		envNetns = windowsNetNS
	}

	createSucceeded = true
	env := &RunnerEnv{
		ID:        id,
		Provider:  cfg.Provider,
		Repo:      cfg.Repo,
		Netns:     envNetns,
		RunnerDir: jobRunnerDir,
		Dind:      dindServer,
		Container: container,
		Task:      task,
	}
	if r.cfg.OnTaskStarted != nil {
		r.cfg.OnTaskStarted(env)
	}
	return env, nil
}

// Destroy tears down a runner environment completely.
func (r *Runtime) Destroy(ctx context.Context, env *RunnerEnv) error {
	ctx = namespaces.WithNamespace(ctx, namespace)

	r.cfg.Log.Info("destroying runner environment", "id", env.ID)

	if r.cfg.OnTaskDestroy != nil {
		r.cfg.OnTaskDestroy(env)
	}

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
		// Drop the runner's intra-job container-to-container allows first, so a
		// port number the kernel later reuses cannot inherit a stale reach.
		r.cfg.Network.LeaveJobNetwork(env.ID)
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
	if env.RunnerDir != "" {
		if err := os.RemoveAll(env.RunnerDir); err != nil {
			r.cfg.Log.Warn("failed to remove job runner dir", "id", env.ID, "path", env.RunnerDir, "error", err)
		}
	}

	// Remove the per-job workdir <DataDir>/jobs/<id>/ in full. dind.Server.Stop
	// (called above via env.Dind.Stop) only removes the docker/ subdir, leaving
	// the parent jobs/<id>/ — plus the runner's _work tree and any extracted
	// php-sdk — behind. On Windows those dirs were leaking indefinitely; this
	// is the primary per-job cleanup that stops the leak. Retries to ride out a
	// Windows sharing violation from the just-exited container's file handles.
	removeJobWorkdir(r.cfg.DataDir, env.ID, r.cfg.Log)

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

// withRunnerMount bind-mounts a per-job copy of the runner directory into the
// container rw. The runner needs write access (e.g. run-helper.sh at startup)
// so it cannot use the shared extracted dir directly; the caller provides a
// job-specific copy.
//
// This is the Windows path (see prepareJobRunnerTree): the copy is a full byte
// copy with independent inodes, so a job's writes cannot reach the shared tree
// or another job. On Linux the runner tree is instead an overlayfs with the
// shared tree as a read-only lowerdir (withRunnerOverlay) — an rw bind of a
// hardlinked copy there was the F1 cross-job poisoning vector.
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

// withRunnerOverlay mounts the shared runner tree into the container as an
// overlayfs: the shared, persistent runner binaries are the READ-ONLY
// lowerdir, and each job gets its own private upperdir+workdir on top. Every
// write a job makes under the runner mount — including an in-place mutation
// like `echo x >> /actions-runner/run.sh`, or the runner's own regeneration
// of run-helper.sh at startup — is copied up into that job's upperdir and can
// never touch the shared lower inodes. This is the F1 fix: the previous
// `cp -al` hardlink copy shared inodes with the persistent tree, so a job
// running as container-root (== host root, no userns) could poison the
// runner tree for every concurrent and future job.
//
// lowerdir is the shared extracted runner directory (RunnerDir). upperdir and
// workdir are per-job directories the caller created on the same filesystem
// as the upperdir (overlayfs requires upper and work to share a filesystem).
// The host already runs the overlayfs containerd snapshotter, so overlay
// support on the data filesystem is guaranteed.
//
// Linux only. Windows containers cannot mount overlayfs; that path keeps the
// per-job byte copy (copyDirNative), which already gives each job independent
// inodes — see prepareJobRunnerTree.
func withRunnerOverlay(lowerdir, upperdir, workdir, containerDir string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: containerDir,
			Type:        "overlay",
			Source:      "overlay",
			Options: []string{
				// index=off keeps things simple and avoids the copy-up index
				// that can conflict when the same lowerdir is reused across
				// many concurrent per-job overlays (one per job, same lower).
				"index=off",
				"lowerdir=" + lowerdir,
				"upperdir=" + upperdir,
				"workdir=" + workdir,
			},
		})
		return nil
	}
}

// withCacheProxyMounts adds the bind mounts a cache proxy needs in every job
// container. The sources are host directories ephemerd generates and owns;
// runc creates the destination if the image does not have it.
func withCacheProxyMounts(mounts []proxies.Mount) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		for _, m := range mounts {
			if m.Source == "" || m.Destination == "" {
				continue
			}
			if goruntime.GOOS == "windows" {
				opts := []string{"rw"}
				if m.ReadOnly {
					opts = []string{"ro"}
				}
				s.Mounts = append(s.Mounts, ocispec.Mount{
					Destination: m.Destination,
					Source:      m.Source,
					Options:     opts,
				})
				continue
			}
			opts := []string{"rbind", "rw"}
			if m.ReadOnly {
				// "ro" alone is not enough on a recursive bind: without
				// rprivate a later host-side mount could propagate in.
				opts = []string{"rbind", "ro", "rprivate"}
			}
			s.Mounts = append(s.Mounts, ocispec.Mount{
				Destination: m.Destination,
				Type:        "bind",
				Source:      m.Source,
				Options:     opts,
			})
		}
		return nil
	}
}

// prepareJobRunnerTree materializes the per-job runner directory rooted at
// jobRunnerDir and returns (1) the SpecOpt that mounts the shared runner tree
// at containerDir for this job and (2) the host path holding the job's
// WRITABLE copy of the tree — what dind must resolve sibling `docker -v`
// sources against.
//
// Linux (and other Unix): an overlayfs. The shared, persistent runnerDir is
// the READ-ONLY lowerdir; each job gets a private upperdir+workdir. A job
// running as container-root can freely write under the mount, but every write
// (including in-place mutations of shared files, and the runner regenerating
// run-helper.sh at startup) is copied up into that job's upperdir and can
// never mutate the shared lower inodes. This is the F1 containment fix: the
// previous `cp -al` copy shared inodes with the persistent tree, so one job
// could poison the runner binaries for every concurrent and future job. The
// writable host path is the upperdir — everything the runner creates under
// the mount (notably _work/) lands there, which is exactly where a sibling
// `docker -v /actions-runner/_work/...` must resolve on the host.
//
// Windows: a per-job byte copy (copyDirNative). Windows containers cannot
// mount overlayfs, but the byte copy already gives each job independent
// inodes, so an in-place write cannot reach the shared tree or another job.
// The writable host path is the copy itself.
func prepareJobRunnerTree(runnerDir, jobRunnerDir, containerDir string) (writableHostDir string, opt oci.SpecOpts, err error) {
	if goruntime.GOOS == "windows" {
		if cerr := copyDirForJob(runnerDir, jobRunnerDir); cerr != nil {
			return "", nil, cerr
		}
		return jobRunnerDir, withRunnerMount(jobRunnerDir, containerDir), nil
	}
	// Remove any stale directory from a crashed job that reused this id so a
	// leftover upper can't seed the new job's writable layer.
	if rerr := os.RemoveAll(jobRunnerDir); rerr != nil {
		return "", nil, rerr
	}
	upper := filepath.Join(jobRunnerDir, "upper")
	work := filepath.Join(jobRunnerDir, "work")
	if merr := os.MkdirAll(upper, 0o755); merr != nil {
		return "", nil, merr
	}
	if merr := os.MkdirAll(work, 0o755); merr != nil {
		return "", nil, merr
	}
	return upper, withRunnerOverlay(runnerDir, upper, work, containerDir), nil
}

// copyDirForJob creates a writable byte copy of src at dst for a single job.
// Used on Windows, where overlayfs is unavailable; each job therefore gets an
// independent copy with its own inodes (no shared-inode mutation across jobs).
// It uses a native Go walk+copy — xcopy returned exit 4 (init error)
// intermittently when invoked under the SYSTEM service account, even though
// manual runs from the same user worked. Going native avoids the
// external-command dependency and surfaces real I/O errors.
//
// Linux no longer copies the runner tree at all: prepareJobRunnerTree mounts
// it as an overlay with the shared tree as a read-only lowerdir. The earlier
// Linux implementation here used `cp -al` (hardlinks), which shared inodes
// with the persistent tree and was the F1 cross-job poisoning vector.
func copyDirForJob(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return copyDirNative(src, dst)
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
		"ephpm/ephemerd:runner-ci-linux",
		"docker.io/ephpm/ephemerd:runner-ci-linux",
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
