package dind

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
)

// BuildScopePrefix returns the image-name prefix every build result from
// jobID is stored under in the shared buildkit containerd namespace. It is
// the prefix half of scopedBuildRef.
//
// Empty jobID returns "" — callers must treat that as "matches nothing",
// never as "matches everything", or a teardown would wipe the whole
// namespace.
func BuildScopePrefix(jobID string) string {
	if jobID == "" {
		return ""
	}
	return buildScopeRegistry + "/" + strings.ToLower(jobID) + "/"
}

// buildRecordJobID extracts the job scope from an image record name in the
// buildkit namespace, reporting false for records that are not job-scoped
// build output (BuildKit's own cache records, anything staged there by
// other tooling). Pure — unit-tested without containerd.
func buildRecordJobID(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, buildScopeRegistry+"/")
	if !ok {
		return "", false
	}
	jobID, tail, ok := strings.Cut(rest, "/")
	if !ok || jobID == "" || tail == "" {
		return "", false
	}
	return jobID, true
}

// DeadBuildRecordGrace is how recently a job-scoped record may have been
// written and still be exempt from the dead-job sweep.
//
// The grace closes a TOCTOU in the sweep's liveness check: the live set is a
// container listing captured BEFORE the record listing, so a job that starts
// — and exports a build — between the two listings has records the stale
// live set would judge dead, and the sweep would delete a live job's build
// output mid-job (breaking its subsequent docker push, and any containerd GC
// the synchronous delete triggers runs against the job's in-flight solve).
// A record can only be at risk if it was written after the live set was
// captured, i.e. within the last pass; anything younger than the grace is
// therefore skipped and picked up by a later tick once its job is provably
// gone. Same principle as runtime.ReapDeadContainers' grace window: never
// collect a resource whose owning operation could still be live.
const DeadBuildRecordGrace = 15 * time.Minute

// BuildRecord is one image record in the buildkit namespace as the dead-job
// sweep sees it: its name and when containerd last wrote it.
type BuildRecord struct {
	Name string
	// UpdatedAt is the record's last containerd write (falling back to its
	// creation time when it was never updated). Zero means the age is
	// unknown.
	UpdatedAt time.Time
}

// DeadBuildRecords returns the job-scoped build result records in records
// whose job is not in live and whose last write is older than grace.
//
// This is the decision half of the buildkit-namespace leak fix, kept pure
// so the "which records are garbage" rule is testable on its own.
//
// Why these records are garbage by construction: every `docker build` in a
// job exports its image into the ONE shared buildkit namespace under a
// name scoped by that job's unique ID (see scopedBuildRef — the scoping
// exists to stop concurrent jobs racing on the same tag). Because the ID is
// unique per job, nothing ever overwrites or reuses those names, and
// nothing deleted them either. A production node accumulated 76 such
// records across 49 long-dead jobs, whose gc.flat leases then pinned ~44 GB
// of content that containerd's GC could never reclaim.
//
// live holds container IDs as ephemerd knows them; matching is done on the
// lowercased form because scopedBuildRef lowercases the ID to satisfy
// Docker's reference grammar. Records younger than grace — or with no
// usable timestamp, since an unknown age must never satisfy an "older than"
// test — are never selected; see DeadBuildRecordGrace for why.
func DeadBuildRecords(records []BuildRecord, live map[string]struct{}, now time.Time, grace time.Duration) []string {
	lowerLive := make(map[string]struct{}, len(live))
	for id := range live {
		lowerLive[strings.ToLower(id)] = struct{}{}
	}
	var out []string
	for _, rec := range records {
		jobID, ok := buildRecordJobID(rec.Name)
		if !ok {
			continue
		}
		if _, alive := lowerLive[jobID]; alive {
			continue
		}
		if rec.UpdatedAt.IsZero() || now.Sub(rec.UpdatedAt) <= grace {
			continue
		}
		out = append(out, rec.Name)
	}
	return out
}

// CleanupJobBuildRecords deletes the build result records job jobID left in
// the shared buildkit namespace. Called from Server.Stop, so a job's build
// output is released as soon as the job ends.
//
// It deliberately does NOT touch BuildKit's own cache records, leases or
// snapshots. Those belong to BuildKit's bbolt cache DB, which keeps its own
// references to them; deleting them behind BuildKit's back leaves the
// snapshots pinned (reclaiming nothing) and corrupts the cache index.
// Bounding that half is BuildKit's GC policy's job — see
// buildkit.GCConfig, which ephemerd previously left unset.
//
// Returns the number of records deleted.
func CleanupJobBuildRecords(ctx context.Context, c *client.Client, buildkitNS, jobID string, log *slog.Logger) int {
	prefix := BuildScopePrefix(jobID)
	if c == nil || buildkitNS == "" || prefix == "" {
		return 0
	}
	nsCtx := namespaces.WithNamespace(ctx, buildkitNS)
	imgs, err := c.ImageService().List(nsCtx)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			log.Warn("buildkit cleanup: list images", "namespace", buildkitNS, "error", err)
		}
		return 0
	}

	deleted := 0
	for _, img := range imgs {
		if !strings.HasPrefix(img.Name, prefix) {
			continue
		}
		if derr := deleteBuildRecord(nsCtx, c, img.Name); derr != nil {
			log.Warn("buildkit cleanup: image delete",
				"namespace", buildkitNS, "image", img.Name, "error", derr)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		log.Info("buildkit cleanup: removed job build records",
			"namespace", buildkitNS, "job_id", jobID, "count", deleted)
	}
	return deleted
}

// PruneDeadBuildRecords removes every job-scoped build record in the
// buildkit namespace whose job is not in live. It is the sweep that catches
// what CleanupJobBuildRecords misses: jobs killed by SIGKILL, a host reboot,
// or a daemon that predates per-job cleanup entirely.
//
// Safe to run while jobs are in flight — records belonging to a live job
// are skipped, and records written more recently than DeadBuildRecordGrace
// are left for a later tick because their job may have started after live
// was captured.
func PruneDeadBuildRecords(ctx context.Context, c *client.Client, buildkitNS string, live map[string]struct{}, log *slog.Logger) (int, error) {
	if c == nil || buildkitNS == "" {
		return 0, nil
	}
	nsCtx := namespaces.WithNamespace(ctx, buildkitNS)
	imgs, err := c.ImageService().List(nsCtx)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	records := make([]BuildRecord, 0, len(imgs))
	for _, img := range imgs {
		rec := BuildRecord{Name: img.Name, UpdatedAt: img.UpdatedAt}
		if rec.UpdatedAt.IsZero() {
			rec.UpdatedAt = img.CreatedAt
		}
		records = append(records, rec)
	}

	deleted := 0
	for _, name := range DeadBuildRecords(records, live, time.Now(), DeadBuildRecordGrace) {
		if derr := deleteBuildRecord(nsCtx, c, name); derr != nil {
			log.Warn("buildkit cleanup: image delete",
				"namespace", buildkitNS, "image", name, "error", derr)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		log.Info("buildkit cleanup: removed dead job build records",
			"namespace", buildkitNS, "count", deleted, "live_jobs", len(live))
	}
	return deleted, nil
}

// deleteBuildRecord removes one image record synchronously so containerd's
// content GC releases the blobs before we return — the caller may be about
// to measure free space.
func deleteBuildRecord(nsCtx context.Context, c *client.Client, name string) error {
	err := c.ImageService().Delete(nsCtx, name, images.SynchronousDelete())
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}
