//go:build linux

package cni

import (
	"archive/tar"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

//go:embed all:embed
var cniFS embed.FS

// Version is set at build time via ldflags.
var Version = "unknown"

// Manager handles extraction and caching of embedded CNI plugin binaries.
type Manager struct {
	dataDir string
	log     *slog.Logger
}

// New creates a CNI plugin manager.
func New(dataDir string, log *slog.Logger) *Manager {
	return &Manager{dataDir: dataDir, log: log}
}

// Dir returns the path where CNI plugin binaries are extracted.
func (m *Manager) Dir() string {
	return filepath.Join(m.dataDir, "cni", "bin", Version)
}

// Extract extracts the embedded CNI plugins tarball. No-op if already cached.
func (m *Manager) Extract() error {
	dir := m.Dir()

	marker := filepath.Join(dir, ".extracted")
	if _, err := os.Stat(marker); err == nil {
		m.log.Debug("CNI plugins already extracted", "dir", dir, "version", Version)
		return nil
	}

	m.log.Info("extracting embedded CNI plugins", "version", Version, "dir", dir)

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("cleaning CNI dir: %w", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating CNI dir: %w", err)
	}

	tarballName, err := m.findTarball()
	if err != nil {
		return err
	}

	f, err := cniFS.Open(tarballName)
	if err != nil {
		return fmt.Errorf("opening embedded CNI plugins: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := extractTarGz(f, dir); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("extracting CNI plugins: %w", err)
	}

	if err := os.WriteFile(marker, []byte(Version), 0o644); err != nil {
		return fmt.Errorf("writing extraction marker: %w", err)
	}

	m.log.Info("CNI plugins extracted", "version", Version, "dir", dir)
	return nil
}

func (m *Manager) findTarball() (string, error) {
	entries, err := cniFS.ReadDir("embed")
	if err != nil {
		return "", fmt.Errorf("reading embedded files: %w", err)
	}

	for _, e := range entries {
		name := e.Name()
		if name == ".gitkeep" {
			continue
		}
		return filepath.Join("embed", name), nil
	}

	return "", fmt.Errorf("no CNI plugins archive found in embedded files (did you run 'make download-cni'?)")
}

func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}

		target := filepath.Join(dest, hdr.Name)

		if !filepath.IsLocal(hdr.Name) {
			return fmt.Errorf("invalid path in archive: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("creating dir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("creating parent dir for %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("creating file %s: %w", target, err)
			}
			if _, copyErr := io.Copy(f, tr); copyErr != nil {
				if err := f.Close(); err != nil {
					return fmt.Errorf("closing file %s after write error: %w (write: %w)", target, err, copyErr)
				}
				return fmt.Errorf("writing file %s: %w", target, copyErr)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("closing file %s: %w", target, err)
			}
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("creating symlink %s: %w", target, err)
			}
		}
	}

	return nil
}
