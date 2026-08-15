package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/cacheprune"
	"github.com/urfave/cli/v3"
)

// cacheEntry describes a single on-disk cache that ephemerd creates and
// manages under its data directory. Each entry knows its own root path
// (relative to the data dir), a human description, and whether clearing it
// while the daemon is running is safe.
//
// The root is expressed relative to the data dir (not absolute) so the
// resolved absolute path can always be validated as being *under* the data
// dir before any removal — this is the path-traversal guard that keeps a
// misconfigured or malicious cache name from turning `cache clear` into an
// `rm -rf` outside the data root. See resolveCacheRoot and clearCacheDir.
type cacheEntry struct {
	// Name is the operator-facing cache identifier (e.g. "images").
	Name string
	// Rel is the path relative to the data dir where this cache lives.
	Rel string
	// Description is a one-line summary shown in `cache list`.
	Description string
	// LiveSafe reports whether the cache can be cleared while the daemon
	// is running without corrupting live state. Caches backing containerd
	// or an in-flight VM/build are NOT live-safe.
	LiveSafe bool
	// KeepDirs, when set, are immediate child directory names that are
	// preserved when clearing (e.g. the "embed" assets under vm/). Their
	// siblings are removed. When empty, the whole cache root's contents
	// are removed but the root directory itself is recreated empty.
	KeepDirs []string
}

// managedCaches returns every on-disk cache ephemerd manages, in a stable
// order. Paths are relative to the data dir; resolveCacheRoot turns them
// into validated absolute paths.
//
// NOTE: the per-repo dind image cache is NOT in this list — it lives in
// containerd metadata namespaces (ephemerd-dind-cache-*), not a plain
// directory, so it cannot be sized or cleared by a filesystem walk. It is
// pruned by the running daemon (see dind.CachePrune / cache_prune_interval)
// and reported separately by `cache list` for visibility.
func managedCaches() []cacheEntry {
	return []cacheEntry{
		{
			Name:        "images",
			Rel:         "images",
			Description: "Staged OCI image tarballs imported into containerd on startup",
			LiveSafe:    true,
		},
		{
			Name:        "gomod",
			Rel:         filepath.Join("cache", "gomod"),
			Description: "Go module proxy cache (GOPROXY) served to job containers",
			LiveSafe:    true,
		},
		{
			Name:        "buildkit",
			Rel:         "buildkit",
			Description: "Embedded BuildKit solver cache + history (docker build layers)",
			LiveSafe:    false,
		},
		{
			Name:        "worker",
			Rel:         "worker",
			Description: "BuildKit worker snapshot/content root",
			LiveSafe:    false,
		},
		{
			Name:        "runners",
			Rel:         "runners",
			Description: "Extracted GitHub Actions runner binaries (per version/OS)",
			LiveSafe:    false,
		},
		{
			Name:        "cni",
			Rel:         "cni",
			Description: "Extracted CNI plugin binaries (per version)",
			LiveSafe:    false,
		},
		{
			Name:        "artifacts",
			Rel:         "artifacts",
			Description: "OCI artifact layers extracted for macOS VM jobs (per job)",
			LiveSafe:    true,
		},
		{
			Name:        "vm",
			Rel:         "vm",
			Description: "VM images and per-job clones (macOS base.img, Linux VM, run dirs)",
			LiveSafe:    false,
			// Preserve embedded assets and the cross-compiled Linux binary
			// staged for WSL — those are not caches, they're shipped state.
			KeepDirs: []string{"embed"},
		},
		{
			Name:        "containerd",
			Rel:         "containerd",
			Description: "containerd content store, snapshots, and state (backs all images)",
			LiveSafe:    false,
		},
		{
			Name: "jobs",
			Rel:  "jobs",
			Description: "Per-job runner workdirs (_work, extracted php-sdk, dind sockets); " +
				"in-flight jobs are skipped",
			// Live-safe, BUT clearing must skip the workdir of any job that is
			// still running — see cacheClearCmd, which folds the running jobs'
			// directory names into KeepDirs before calling clearCacheDir. The
			// per-child path-root guard in clearCacheDir still applies, so a
			// job dir is never removed from outside <data>/jobs/.
			LiveSafe: true,
		},
	}
}

// cacheByName returns the managed cache with the given name, or false.
func cacheByName(name string) (cacheEntry, bool) {
	for _, c := range managedCaches() {
		if c.Name == name {
			return c, true
		}
	}
	return cacheEntry{}, false
}

// resolveCacheRoot joins the cache's relative path onto dataDir and verifies
// the result stays under dataDir. It returns an error for any path that
// escapes the data root (defence against a "../" in a cache Rel or a symlink
// pointing outside). This is the single choke point every clear MUST pass
// through before removing anything.
func resolveCacheRoot(dataDir string, c cacheEntry) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("data dir is empty")
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolving data dir %q: %w", dataDir, err)
	}
	target := filepath.Join(root, c.Rel)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolving cache path %q: %w", target, err)
	}
	if !isUnder(absTarget, root) {
		return "", fmt.Errorf("cache %q resolves to %q which is outside the data dir %q", c.Name, absTarget, root)
	}
	return absTarget, nil
}

// isUnder reports whether target is root itself or a descendant of root.
// Uses filepath.Rel so it is correct on both / and \ separators and is not
// fooled by shared string prefixes (e.g. /var/lib/ephemerd2 under
// /var/lib/ephemerd).
func isUnder(target, root string) bool {
	if target == root {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// Any path that has to climb out of root (starts with "..") is not under it.
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// dirSize returns the total size in bytes of all regular files under path.
// A missing path reports 0 with no error (an absent cache is simply empty).
// Symlinks are not followed — only the on-disk bytes rooted at path count,
// so a symlink into another cache can't inflate or double-count sizes.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			if os.IsNotExist(ierr) {
				return nil
			}
			return ierr
		}
		total += info.Size()
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return total, err
	}
	return total, nil
}

// humanBytes formats a byte count as a human-readable string using binary
// (1024-based) units, matching how operators reason about disk (KiB/MiB/GiB
// shown as KB/MB/GB for brevity, consistent with `du -h`-style output).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// clearCacheDir removes the contents of a validated cache root. The root
// itself is recreated empty (so the daemon can repopulate it), and any
// KeepDirs children are preserved. The caller MUST pass a path already
// vetted by resolveCacheRoot — clearCacheDir re-checks the under-root
// invariant per child as belt-and-suspenders before every RemoveAll.
//
// Returns the number of bytes freed (best-effort, measured before removal).
func clearCacheDir(dataDir, absRoot string, keep []string) (int64, error) {
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return 0, fmt.Errorf("resolving data dir: %w", err)
	}
	// Guard: never operate on the data dir root itself or anything outside it.
	if absRoot == root {
		return 0, fmt.Errorf("refusing to clear the data dir root %q", root)
	}
	if !isUnder(absRoot, root) {
		return 0, fmt.Errorf("refusing to clear %q: outside data dir %q", absRoot, root)
	}

	freed, _ := dirSize(absRoot)

	entries, err := os.ReadDir(absRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // nothing to clear
		}
		return 0, fmt.Errorf("reading cache dir %q: %w", absRoot, err)
	}

	keepSet := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}

	for _, e := range entries {
		if _, skip := keepSet[e.Name()]; skip {
			continue
		}
		child := filepath.Join(absRoot, e.Name())
		// Re-validate every child before removal. This is redundant with
		// the root check above but makes the RemoveAll provably in-bounds
		// even if absRoot were ever passed unchecked.
		if !isUnder(child, root) {
			return freed, fmt.Errorf("refusing to remove %q: outside data dir %q", child, root)
		}
		if err := os.RemoveAll(child); err != nil {
			return freed, fmt.Errorf("removing %q: %w", child, err)
		}
	}
	return freed, nil
}

// daemonRunning reports whether an ephemerd daemon is currently reachable on
// the control socket. Used to refuse clearing non-live-safe caches out from
// under a running daemon.
func daemonRunning(ctx context.Context) bool {
	cc, err := dialControl(ctx)
	if err != nil {
		return false
	}
	defer func() { _ = cc.Close() }()
	_, err = cc.Status(ctx, &apiv1.StatusRequest{})
	return err == nil
}

// daemonPrunable reports whether a cache is one the running daemon can
// prune through the manager that owns it (see pkg/cacheprune). Pure —
// split out so the routing rule is testable without a daemon.
func daemonPrunable(name string) bool {
	for _, t := range cacheprune.AllTargets() {
		if t == name {
			return true
		}
	}
	return false
}

// splitDaemonPrunable partitions cache entries into the ones the daemon can
// prune and the ones that still need the filesystem sweep. Pure.
func splitDaemonPrunable(targets []cacheEntry) (viaDaemon, viaFilesystem []cacheEntry) {
	for _, c := range targets {
		if daemonPrunable(c.Name) {
			viaDaemon = append(viaDaemon, c)
		} else {
			viaFilesystem = append(viaFilesystem, c)
		}
	}
	return viaDaemon, viaFilesystem
}

// pruneViaDaemon asks the running daemon to prune whichever targets it
// manages, prints what came back, and returns the targets the caller must
// still handle on the filesystem.
//
// A per-target failure is reported and does NOT fall back to deleting the
// directory: for the BuildKit cache that would leave its bbolt index
// pointing at content that is gone. Only an explicit --offline does that.
func pruneViaDaemon(ctx context.Context, targets []cacheEntry) ([]cacheEntry, error) {
	viaDaemon, viaFilesystem := splitDaemonPrunable(targets)
	if len(viaDaemon) == 0 {
		return viaFilesystem, nil
	}

	cc, err := dialControl(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to the running daemon: %w", err)
	}
	defer func() { _ = cc.Close() }()

	names := make([]string, len(viaDaemon))
	for i, c := range viaDaemon {
		names[i] = c.Name
	}
	// all=true: "clear" means clear. The daemon still refuses to touch
	// anything a running job holds — that safety comes from the cache
	// managers, not from keeping cache around.
	resp, err := cc.PruneCache(ctx, &apiv1.PruneCacheRequest{Targets: names, All: true})
	if err != nil {
		return nil, fmt.Errorf("asking the daemon to prune %s: %w", strings.Join(names, ", "), err)
	}

	var totalFreed int64
	var failed []string
	for _, r := range resp.GetResults() {
		if e := r.GetError(); e != "" {
			fmt.Printf("failed  %-12s %s\n", r.GetName(), e)
			failed = append(failed, r.GetName())
			continue
		}
		totalFreed += r.GetBytesFreed()
		fmt.Printf("pruned  %-12s (%s freed, %d record(s)) via daemon\n",
			r.GetName(), humanBytes(r.GetBytesFreed()), r.GetRecordsRemoved())
	}
	if len(failed) > 0 {
		return nil, fmt.Errorf("daemon could not prune: %s (pass --offline to clear on disk with the daemon stopped)",
			strings.Join(failed, ", "))
	}
	if len(viaFilesystem) == 0 {
		fmt.Printf("Done. Freed %s.\n", humanBytes(totalFreed))
	}
	return viaFilesystem, nil
}

// runningJobDirs returns the immediate child directory names under
// <data>/jobs/ that belong to a job the daemon is CURRENTLY running, so
// `cache clear jobs` can skip them. Each job's on-disk workdir is named after
// its runner/container ID (RunnerEnv.ID), which the control API reports as the
// job's Name (see scheduler.controlServer.toProto and dind.New, which roots
// the per-job dir at <data>/jobs/<JobID>/).
//
// The set is queried from the running daemon via the control socket. If the
// daemon is not reachable, there are no in-flight jobs by definition — every
// jobs/* dir is a leftover from a previous run — so we return an empty set and
// the caller clears them all. A control error (daemon up but ListJobs failed)
// is surfaced so we fail safe rather than risk deleting a live job's dir.
func runningJobDirs(ctx context.Context) (map[string]struct{}, error) {
	cc, err := dialControl(ctx)
	if err != nil {
		// Daemon unreachable: nothing is running, so nothing to skip.
		return map[string]struct{}{}, nil
	}
	defer func() { _ = cc.Close() }()

	// A dial success does not guarantee the daemon is up (grpc.NewClient is
	// lazy). Probe with Status first; if that fails the daemon is down and
	// there are no running jobs to protect.
	if _, err := cc.Status(ctx, &apiv1.StatusRequest{}); err != nil {
		return map[string]struct{}{}, nil
	}

	resp, err := cc.ListJobs(ctx, &apiv1.ListJobsRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing running jobs to protect their workdirs: %w", err)
	}
	names := make(map[string]struct{}, len(resp.GetJobs()))
	for _, j := range resp.GetJobs() {
		if n := j.GetName(); n != "" {
			names[n] = struct{}{}
		}
	}
	return names, nil
}

// sortedKeys returns the keys of set in stable, sorted order. Used so the
// jobs-cache skip list is deterministic (nice for logging/tests).
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cacheCmd() *cli.Command {
	return &cli.Command{
		Name:  "cache",
		Usage: "Inspect and clear ephemerd's on-disk caches",
		Description: "Lists every cache ephemerd manages under its data dir with sizes, " +
			"and selectively clears them. Caches that back a running daemon " +
			"(containerd, buildkit, VM images) are only cleared when the daemon " +
			"is stopped, unless --force is given.",
		Commands: []*cli.Command{
			cacheListCmd(),
			cacheClearCmd(),
		},
	}
}

// cacheReport is one row of `cache list`.
type cacheReport struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size_bytes"`
	SizeHuman   string `json:"size"`
	LiveSafe    bool   `json:"live_safe"`
	Description string `json:"description"`
}

func cacheListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List all managed caches with their sizes",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "emit machine-readable JSON"},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			reports := make([]cacheReport, 0, len(managedCaches()))
			var total int64
			for _, c := range managedCaches() {
				root, err := resolveCacheRoot(configDir, c)
				if err != nil {
					return err
				}
				size, err := dirSize(root)
				if err != nil {
					return fmt.Errorf("sizing cache %q: %w", c.Name, err)
				}
				total += size
				reports = append(reports, cacheReport{
					Name:        c.Name,
					Path:        root,
					Size:        size,
					SizeHuman:   humanBytes(size),
					LiveSafe:    c.LiveSafe,
					Description: c.Description,
				})
			}

			if cmd.Bool("json") {
				out := map[string]any{
					"data_dir":    configDir,
					"caches":      reports,
					"total_bytes": total,
					"total":       humanBytes(total),
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			fmt.Printf("Data dir: %s\n\n", configDir)
			fmt.Printf("%-12s %-10s %-10s %s\n", "NAME", "SIZE", "CLEARABLE", "PATH")
			for _, r := range reports {
				clearable := "live"
				if !r.LiveSafe {
					clearable = "stopped"
				}
				fmt.Printf("%-12s %-10s %-10s %s\n", r.Name, r.SizeHuman, clearable, r.Path)
			}
			fmt.Printf("%-12s %-10s\n", "TOTAL", humanBytes(total))
			fmt.Println()
			fmt.Println("CLEARABLE: 'live' = safe to clear while running; 'stopped' = stop the daemon first (or use --force).")
			fmt.Println("Note: the per-repo dind image cache lives in containerd namespaces (ephemerd-dind-cache-*),")
			fmt.Println("      not a directory; it is pruned by the running daemon (see cache_prune_interval).")
			return nil
		},
	}
}

func cacheClearCmd() *cli.Command {
	return &cli.Command{
		Name:      "clear",
		Usage:     "Clear one cache by name, or all clearable caches with --all",
		ArgsUsage: "[name]",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "all", Usage: "clear every clearable cache"},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"force", "y"}, Usage: "skip the confirmation prompt"},
			&cli.BoolFlag{Name: "offline", Usage: "bypass the daemon and clear on the filesystem (escape hatch for a wedged daemon; may not reclaim the BuildKit cache)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			all := cmd.Bool("all")
			force := cmd.Bool("yes")
			name := cmd.Args().First()

			if all && name != "" {
				return fmt.Errorf("pass either a cache name or --all, not both")
			}
			if !all && name == "" {
				return fmt.Errorf("specify a cache name or --all (see 'ephemerd cache list')")
			}

			var targets []cacheEntry
			if all {
				targets = managedCaches()
			} else {
				c, ok := cacheByName(name)
				if !ok {
					return fmt.Errorf("unknown cache %q (see 'ephemerd cache list')", name)
				}
				targets = []cacheEntry{c}
			}

			running := daemonRunning(ctx)

			// Confirmation for destructive clears unless --yes/--force.
			// Asked up front, before the daemon/offline split, so the
			// operator confirms exactly what they typed.
			if !force {
				names := make([]string, len(targets))
				for i, c := range targets {
					names[i] = c.Name
				}
				fmt.Printf("About to clear: %s\n", strings.Join(names, ", "))
				if !confirm("Proceed? [y/N] ") {
					fmt.Println("Aborted.")
					return nil
				}
			}

			// Ask the RUNNING daemon to prune the caches it manages. That
			// is surgical (it knows what is in use), it works live, and
			// for the BuildKit cache it is the ONLY thing that works:
			// BuildKit's bbolt DB holds its own references to the
			// snapshots behind each cache record, so deleting the
			// directory or the containerd records behind its back leaves
			// them pinned and frees nothing.
			//
			// --offline forces the old filesystem sweep as an escape
			// hatch for a wedged daemon.
			if running && !cmd.Bool("offline") {
				remaining, err := pruneViaDaemon(ctx, targets)
				if err != nil {
					return err
				}
				if len(remaining) == 0 {
					return nil
				}
				targets = remaining
			}

			// Determine which targets we can actually clear. Non-live-safe
			// caches are skipped (for --all) or refused (for a named clear)
			// while the daemon runs, unless --force overrides.
			var clearable []cacheEntry
			for _, c := range targets {
				if running && !c.LiveSafe && !force {
					if all {
						fmt.Printf("skipping %q: not safe to clear while the daemon is running (use --force)\n", c.Name)
						continue
					}
					return fmt.Errorf("cache %q is not safe to clear while the daemon is running; stop ephemerd first or pass --force", c.Name)
				}
				clearable = append(clearable, c)
			}
			if len(clearable) == 0 {
				fmt.Println("Nothing to clear.")
				return nil
			}

			var totalFreed int64
			cleared := 0
			for _, c := range clearable {
				root, err := resolveCacheRoot(configDir, c)
				if err != nil {
					return err
				}
				keep := c.KeepDirs
				// The jobs cache is live-safe, but an in-flight job's workdir
				// must never be deleted out from under it. Fold the running
				// jobs' directory names into the keep set so clearCacheDir
				// skips exactly those children (its per-child path-root guard
				// still governs everything it does remove).
				if c.Name == "jobs" {
					protected, perr := runningJobDirs(ctx)
					if perr != nil {
						return fmt.Errorf("clearing cache %q: %w", c.Name, perr)
					}
					if len(protected) > 0 {
						keep = append(append([]string{}, c.KeepDirs...), sortedKeys(protected)...)
						fmt.Printf("skipping %d in-flight job dir(s) under %q\n", len(protected), c.Name)
					}
				}
				freed, err := clearCacheDir(configDir, root, keep)
				if err != nil {
					return fmt.Errorf("clearing cache %q: %w", c.Name, err)
				}
				totalFreed += freed
				cleared++
				fmt.Printf("cleared %-12s (%s freed) %s\n", c.Name, humanBytes(freed), root)
			}
			fmt.Printf("Done. Cleared %d cache(s), freed %s.\n", cleared, humanBytes(totalFreed))
			return nil
		},
	}
}
