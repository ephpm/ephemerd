package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// copyTarballs copies every *.tar file from src into dst, creating dst if
// needed. Existing destination files of the same size are skipped so repeated
// starts don't re-copy multi-gigabyte image tarballs. Returns an error if src
// is set but unreadable; returns nil when src is empty.
func copyTarballs(src, dst string, log *slog.Logger) error {
	if src == "" {
		return nil
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat images-dir %s: %w", src, err)
	}
	if !srcInfo.IsDir() {
		return fmt.Errorf("images-dir %s is not a directory", src)
	}

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("creating images dest %s: %w", dst, err)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("reading images-dir %s: %w", src, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tar") {
			continue
		}

		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		srcStat, err := os.Stat(srcPath)
		if err != nil {
			log.Warn("skipping tarball: stat failed", "path", srcPath, "error", err)
			continue
		}

		if dstStat, err := os.Stat(dstPath); err == nil && dstStat.Size() == srcStat.Size() {
			log.Info("image tarball already present, skipping copy", "name", entry.Name(), "size", dstStat.Size())
			continue
		}

		log.Info("copying image tarball", "src", srcPath, "dst", dstPath, "size", srcStat.Size())
		if err := copyFile(srcPath, dstPath); err != nil {
			return fmt.Errorf("copying %s: %w", entry.Name(), err)
		}
	}

	return nil
}
