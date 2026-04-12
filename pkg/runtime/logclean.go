package runtime

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanOldLogs removes job log files older than maxAge from the log directory.
// Called on startup and periodically to prevent unbounded disk usage.
func CleanOldLogs(logDir string, maxAge time.Duration, log *slog.Logger) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return // directory may not exist yet
	}

	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			log.Debug("failed to stat log file", "name", entry.Name(), "error", err)
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(logDir, entry.Name())
			if err := os.Remove(path); err != nil {
				log.Debug("failed to remove old log", "path", path, "error", err)
			} else {
				log.Debug("removed old job log", "path", path, "age", time.Since(info.ModTime()).Round(time.Minute))
			}
		}
	}
}
