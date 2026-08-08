package upgrade

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// extractBinary extracts the single ephemerd binary entry from a release
// archive to dest (mode 0755). Windows releases are a .zip containing
// ephemerd.exe; linux/darwin releases are a .tar.gz containing ephemerd.
func extractBinary(archivePath, goos, dest string) error {
	want := binaryEntryName(goos)
	if goos == "windows" {
		return extractZipEntry(archivePath, want, dest)
	}
	return extractTarGzEntry(archivePath, want, dest)
}

// writeBinary copies r into dest with executable permissions.
func writeBinary(r io.Reader, dest string) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil { //nolint:gosec // release archive, bounded size
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func extractZipEntry(archivePath, entry, dest string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if filepath.Base(f.Name) != entry || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open zip entry %s: %w", f.Name, err)
		}
		err = writeBinary(rc, dest)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("extract %s: %w", entry, err)
		}
		return nil
	}
	return fmt.Errorf("no %s entry in archive", entry)
}

func extractTarGzEntry(archivePath, entry, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("no %s entry in archive", entry)
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != entry {
			continue
		}
		if err := writeBinary(tr, dest); err != nil {
			return fmt.Errorf("extract %s: %w", entry, err)
		}
		return nil
	}
}
