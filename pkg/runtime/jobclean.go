package runtime

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// jobsDirName is the immediate child of the data dir under which every job's
// per-job workdir lives: <data>/jobs/<jobID>/. dind roots its socket + temp
// layers there (see pkg/dind.New) and the runner writes its _work tree there,
// so the directory outlives the container and MUST be removed when the job
// finishes or it accumulates (observed on Windows: 100+ leftover dirs, some
// holding a stale php-sdk that blocked a build).
const jobsDirName = "jobs"

// jobWorkdir returns the on-disk workdir for a job: <dataDir>/jobs/<jobID>/.
func jobWorkdir(dataDir, jobID string) string {
	return filepath.Join(dataDir, jobsDirName, jobID)
}

// removeJobWorkdir deletes a single job's workdir (<dataDir>/jobs/<jobID>/) when
// the job completes - success OR failure. It is the primary fix for the job-dir
// leak: the old code only removed the dind docker/ subdirectory (in
// dind.Server.Stop), leaving the parent jobs/<jobID>/ behind forever.
//
// On Windows the just-exited Hyper-V-isolated container can briefly keep handles
// open on files under the dir (the fake-docker socket, temp layers), so a bare
// os.RemoveAll can fail with a sharing violation. We retry with a short backoff
// to ride out that window. A dataDir or jobID of "" is a no-op so callers don't
// have to guard.
func removeJobWorkdir(dataDir, jobID string, log *slog.Logger) {
	if dataDir == "" || jobID == "" {
		return
	}
	dir := jobWorkdir(dataDir, jobID)
	if err := removeAllWithRetry(dir); err != nil {
		if log != nil {
			log.Warn("failed to remove job workdir", "job_id", jobID, "path", dir, "error", err)
		}
		return
	}
	if log != nil {
		log.Debug("removed job workdir", "job_id", jobID, "path", dir)
	}
}

// removeAllWithRetry is os.RemoveAll with a bounded retry, for the Windows case
// where a freshly-exited process may still hold a handle on a file in the tree
// (ERROR_SHARING_VIOLATION / "The process cannot access the file..."). A missing
// path is success. On non-Windows the first attempt almost always succeeds, so
// the retry loop simply doesn't re-enter.
func removeAllWithRetry(dir string) error {
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		err = os.RemoveAll(dir)
		if err == nil {
			return nil
		}
		if os.IsNotExist(err) {
			return nil
		}
		// Backoff grows 100ms, 200ms, 300ms, 400ms - total under a second,
		// long enough for a container's file handles to be released after the
		// task exits without stalling the completion path.
		time.Sleep(time.Duration(i+1) * 100 * time.Millisecond)
	}
	return err
}

// CleanOrphanJobDirs removes leftover per-job workdirs under <dataDir>/jobs/,
// skipping any whose directory name is in keep (the set of currently-running
// job IDs). It is the belt-and-suspenders half of the leak fix: per-job removal
// in Destroy handles the normal path, but a crash / SIGKILL / kill -9 of
// ephemerd (or a job dir left by a version that predates the per-job removal)
// skips Destroy entirely, so we sweep on startup as well.
//
// At worker startup nothing is running yet, so callers pass a nil/empty keep
// and every jobs/* dir is an orphan. keep is provided for symmetry and future
// callers (e.g. a periodic sweep) that must not touch live jobs.
func CleanOrphanJobDirs(dataDir string, keep map[string]struct{}, log *slog.Logger) {
	if dataDir == "" {
		return
	}
	jobsDir := filepath.Join(dataDir, jobsDirName)
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return // directory may not exist yet
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, live := keep[e.Name()]; live {
			continue
		}
		dir := filepath.Join(jobsDir, e.Name())
		if err := removeAllWithRetry(dir); err != nil {
			if log != nil {
				log.Warn("failed to remove orphan job dir", "path", dir, "error", err)
			}
			continue
		}
		if log != nil {
			log.Info("removed orphan job dir", "path", dir)
		}
		removed++
	}
	if removed > 0 && log != nil {
		log.Info("job workdir sweep complete", "removed", removed)
	}
}
