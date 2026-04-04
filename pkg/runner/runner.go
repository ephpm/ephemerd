package runner

import (
	"archive/tar"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
)

//go:embed all:embed
var runnerFS embed.FS

// Runner version embedded at build time.
var Version = "unknown"

// Manager handles extraction and caching of the embedded GitHub Actions runner.
type Manager struct {
	dataDir string
	log     *slog.Logger
}

// New creates a runner manager.
func New(dataDir string, log *slog.Logger) *Manager {
	return &Manager{
		dataDir: dataDir,
		log:     log,
	}
}

// Dir returns the path to the extracted runner directory.
// Call Extract() first to ensure it exists.
func (m *Manager) Dir() string {
	return filepath.Join(m.dataDir, "runners", Version)
}

// Entrypoint returns the runner entrypoint command for the current OS.
func (m *Manager) Entrypoint() string {
	if goruntime.GOOS == "windows" {
		return filepath.Join("C:\\actions-runner", "run.cmd")
	}
	return "/actions-runner/run.sh"
}

// ContainerDir returns the mount target path inside the container.
func (m *Manager) ContainerDir() string {
	if goruntime.GOOS == "windows" {
		return `C:\actions-runner`
	}
	return "/actions-runner"
}

// Extract extracts the embedded runner tarball to the data directory.
// If the runner is already extracted (cached), this is a no-op.
func (m *Manager) Extract() error {
	dir := m.Dir()

	// Check if already extracted
	marker := filepath.Join(dir, ".extracted")
	if _, err := os.Stat(marker); err == nil {
		m.log.Debug("runner already extracted", "dir", dir, "version", Version)
		return nil
	}

	m.log.Info("extracting embedded runner", "version", Version, "dir", dir)

	// Clean up any partial extraction
	os.RemoveAll(dir)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating runner dir: %w", err)
	}

	// Find the embedded tarball
	tarballName, err := m.findTarball()
	if err != nil {
		return err
	}

	f, err := runnerFS.Open(tarballName)
	if err != nil {
		return fmt.Errorf("opening embedded runner: %w", err)
	}
	defer f.Close()

	if err := extractTarGz(f, dir); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("extracting runner: %w", err)
	}

	// Write marker so we skip extraction next time
	if err := os.WriteFile(marker, []byte(Version), 0o644); err != nil {
		return fmt.Errorf("writing extraction marker: %w", err)
	}

	m.log.Info("runner extracted", "version", Version, "dir", dir)
	return nil
}

// findTarball locates the runner archive in the embedded filesystem.
func (m *Manager) findTarball() (string, error) {
	entries, err := runnerFS.ReadDir("embed")
	if err != nil {
		return "", fmt.Errorf("reading embedded files: %w", err)
	}

	for _, e := range entries {
		name := e.Name()
		if name == ".gitkeep" {
			continue
		}
		// Match actions-runner-{os}-{arch}-{version}.tar.gz or .zip
		return filepath.Join("embed", name), nil
	}

	return "", fmt.Errorf("no runner archive found in embedded files (did you run 'make download-runner'?)")
}

func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

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

		// Prevent path traversal
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
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("writing file %s: %w", target, err)
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("creating symlink %s: %w", target, err)
			}
		}
	}

	return nil
}
