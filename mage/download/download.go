package download

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/magefile/mage/mg"
)

const (
	RunnerVersion     = "2.333.1"
	CNIVersion        = "1.6.2"
	ContainerdVersion = "2.2.2"
	RuncVersion       = "1.3.4"
	AlpineVersion     = "3.21.3"

	runnerEmbedDir = "pkg/runner/embed"
	cniEmbedDir    = "pkg/cni/embed"
	shimEmbedDir   = "pkg/containerd/embed"
	vmEmbedDir     = "pkg/vm/embed"
)

// All downloads all assets appropriate for the current OS.
func All() {
	switch runtime.GOOS {
	case "windows":
		mg.Deps(Runner, CNI, Shim)
	default:
		mg.Deps(Runner, CNI, Shim)
	}
}

// Runner downloads the GitHub Actions runner archive for the current platform.
func Runner() error {
	os_, arch := runnerPlatform(runtime.GOOS, runtime.GOARCH)
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	filename := fmt.Sprintf("actions-runner-%s-%s-%s.%s", os_, arch, RunnerVersion, ext)
	dest := filepath.Join(runnerEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", RunnerVersion, filename)
	return downloadFile(url, dest)
}

// RunnerLinux always downloads the Linux x64 runner (for embedding in Windows builds).
func RunnerLinux() error {
	filename := fmt.Sprintf("actions-runner-linux-x64-%s.tar.gz", RunnerVersion)
	dest := filepath.Join(runnerEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", RunnerVersion, filename)
	return downloadFile(url, dest)
}

// RunnerWindows always downloads the Windows x64 runner.
func RunnerWindows() error {
	filename := fmt.Sprintf("actions-runner-win-x64-%s.zip", RunnerVersion)
	dest := filepath.Join(runnerEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", RunnerVersion, filename)
	return downloadFile(url, dest)
}

// CNI downloads the CNI plugins tarball (Linux only, no-op on other OS).
func CNI() error {
	if runtime.GOOS != "linux" {
		fmt.Println("Skipping CNI download (not on Linux)")
		return nil
	}
	arch := cniArch(runtime.GOARCH)
	filename := fmt.Sprintf("cni-plugins-linux-%s-v%s.tgz", arch, CNIVersion)
	dest := filepath.Join(cniEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/v%s/%s", CNIVersion, filename)
	return downloadFile(url, dest)
}

// CNILinux always downloads the Linux amd64 CNI plugins (for cross-compile embed).
func CNILinux() error {
	filename := fmt.Sprintf("cni-plugins-linux-amd64-v%s.tgz", CNIVersion)
	dest := filepath.Join(cniEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/v%s/%s", CNIVersion, filename)
	return downloadFile(url, dest)
}

// Shim downloads containerd-shim-runc-v2 (extracted from tarball) and runc binary.
func Shim() error {
	if runtime.GOOS != "linux" {
		fmt.Println("Skipping shim download (not on Linux)")
		return nil
	}
	if err := downloadShim(); err != nil {
		return err
	}
	return downloadRunc()
}

// ShimLinux always downloads the Linux amd64 shim + runc (for cross-compile embed).
func ShimLinux() error {
	if err := downloadShimForArch("amd64"); err != nil {
		return err
	}
	return downloadRuncForArch("amd64")
}

// Rootfs downloads the Alpine minirootfs for WSL2 Linux VM.
func Rootfs() error {
	filename := fmt.Sprintf("alpine-minirootfs-%s-x86_64.tar.gz", AlpineVersion)
	dest := filepath.Join(vmEmbedDir, filename)
	url := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/releases/x86_64/%s",
		alpineMajorMinor(AlpineVersion), filename)
	return downloadFile(url, dest)
}

func downloadShim() error {
	return downloadShimForArch(cniArch(runtime.GOARCH))
}

func downloadShimForArch(arch string) error {
	dest := filepath.Join(shimEmbedDir, "containerd-shim-runc-v2")
	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}
	if err := os.MkdirAll(shimEmbedDir, 0o755); err != nil {
		return err
	}

	url := fmt.Sprintf("https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-%s.tar.gz",
		ContainerdVersion, ContainerdVersion, arch)
	fmt.Printf("  Downloading containerd-shim-runc-v2 from containerd %s...\n", ContainerdVersion)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download shim: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download shim: HTTP %d", resp.StatusCode)
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip shim: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("containerd-shim-runc-v2 not found in tarball")
		}
		if err != nil {
			return fmt.Errorf("tar shim: %w", err)
		}
		if strings.HasSuffix(hdr.Name, "bin/containerd-shim-runc-v2") {
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			return f.Close()
		}
	}
}

func downloadRunc() error {
	return downloadRuncForArch(cniArch(runtime.GOARCH))
}

func downloadRuncForArch(arch string) error {
	dest := filepath.Join(shimEmbedDir, "runc")
	url := fmt.Sprintf("https://github.com/opencontainers/runc/releases/download/v%s/runc.%s",
		RuncVersion, arch)
	if err := downloadFile(url, dest); err != nil {
		return err
	}
	return os.Chmod(dest, 0o755)
}

// downloadFile downloads url to dest, skipping if the file already exists.
func downloadFile(url, dest string) error {
	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	fmt.Printf("  Downloading %s...\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", dest, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", dest, resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func runnerPlatform(goos, goarch string) (os_, arch string) {
	switch goos {
	case "windows":
		os_ = "win"
	case "darwin":
		os_ = "osx"
	default:
		os_ = "linux"
	}
	if goarch == "arm64" {
		arch = "arm64"
	} else {
		arch = "x64"
	}
	return
}

func cniArch(goarch string) string {
	if goarch == "arm64" {
		return "arm64"
	}
	return "amd64"
}

func alpineMajorMinor(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return version
}
