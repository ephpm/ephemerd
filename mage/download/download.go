package download

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
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
	RunnerVersion        = "2.333.1"
	CNIVersion           = "1.6.2"
	ContainerdVersion    = "2.2.2"
	RuncVersion          = "1.3.4"
	AlpineVersion        = "3.21.3"
	GolangCILintVersion  = "2.11.4"

	runnerEmbedDir = "pkg/runner/embed"
	cniEmbedDir    = "pkg/cni/embed"
	shimEmbedDir   = "pkg/containerd/embed"
	vmEmbedDir     = "pkg/vm/embed"
	toolBinDir     = "bin"
)

// All downloads all assets appropriate for the current OS.
func All() {
	switch runtime.GOOS {
	case "windows":
		mg.Deps(Runner, Cni, Shim)
	default:
		mg.Deps(Runner, Cni, Shim)
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

// Runnerlinux always downloads the Linux x64 runner (for embedding in Windows builds).
func Runnerlinux() error {
	filename := fmt.Sprintf("actions-runner-linux-x64-%s.tar.gz", RunnerVersion)
	dest := filepath.Join(runnerEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", RunnerVersion, filename)
	return downloadFile(url, dest)
}

// Runnerwindows always downloads the Windows x64 runner.
func Runnerwindows() error {
	filename := fmt.Sprintf("actions-runner-win-x64-%s.zip", RunnerVersion)
	dest := filepath.Join(runnerEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", RunnerVersion, filename)
	return downloadFile(url, dest)
}

// Cni downloads the CNI plugins tarball (Linux only, no-op on other OS).
func Cni() error {
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

// Cnilinux always downloads the Linux amd64 CNI plugins (for cross-compile embed).
func Cnilinux() error {
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

// Shimlinux always downloads the Linux amd64 shim + runc (for cross-compile embed).
func Shimlinux() error {
	if err := downloadShimForArch("amd64"); err != nil {
		return err
	}
	return downloadRuncForArch("amd64")
}

// apkPkg describes a package to pre-install in the rootfs.
type apkPkg struct {
	name    string
	version string
	repo    string // "main" or "community"
}

// Packages to pre-install in the rootfs, pinned to Alpine 3.21.3.
// These are the transitive dependencies of gcompat and iptables.
// Update versions when bumping AlpineVersion.
var rootfsPackages = []apkPkg{
	{"musl-obstack", "1.2.3-r2", "main"},
	{"libucontext", "1.3.2-r0", "main"},
	{"gcompat", "1.1.0-r4", "main"},
	{"libmnl", "1.0.5-r2", "main"},
	{"libnftnl", "1.2.8-r0", "main"},
	{"libxtables", "1.8.11-r1", "main"},
	{"iptables", "1.8.11-r1", "main"},
}

// Rootfs builds a custom Alpine rootfs with gcompat and iptables pre-installed.
// Downloads the stock Alpine minirootfs and APK packages from the CDN, then
// combines them into a single tarball — no container runtime required.
func Rootfs() error {
	filename := fmt.Sprintf("ephemerd-rootfs-%s-x86_64.tar.gz", AlpineVersion)
	dest := filepath.Join(vmEmbedDir, filename)

	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}

	if err := os.MkdirAll(vmEmbedDir, 0o755); err != nil {
		return err
	}

	// Remove any old rootfs files to avoid embed conflicts
	for _, pattern := range []string{"alpine-minirootfs-*.tar.gz", "ephemerd-rootfs-*.tar.gz"} {
		oldFiles, _ := filepath.Glob(filepath.Join(vmEmbedDir, pattern))
		for _, f := range oldFiles {
			fmt.Printf("  Removing old rootfs: %s\n", f)
			_ = os.Remove(f)
		}
	}

	// Download base Alpine minirootfs
	baseURL := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/releases/x86_64/alpine-minirootfs-%s-x86_64.tar.gz",
		alpineMajorMinor(AlpineVersion), AlpineVersion)
	fmt.Printf("  Downloading base Alpine minirootfs...\n")
	baseData, err := httpGetBytes(baseURL)
	if err != nil {
		return fmt.Errorf("downloading base rootfs: %w", err)
	}

	// Download APK packages
	pkgData := make([][]byte, len(rootfsPackages))
	for i, pkg := range rootfsPackages {
		url := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/%s/x86_64/%s-%s.apk",
			alpineMajorMinor(AlpineVersion), pkg.repo, pkg.name, pkg.version)
		fmt.Printf("  Downloading %s-%s.apk...\n", pkg.name, pkg.version)
		pkgData[i], err = httpGetBytes(url)
		if err != nil {
			return fmt.Errorf("downloading %s: %w", pkg.name, err)
		}
	}

	// Build combined rootfs tarball
	fmt.Printf("  Building combined rootfs...\n")
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dest)
		}
	}()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Copy all entries from the base rootfs
	baseGz, err := gzip.NewReader(bytes.NewReader(baseData))
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("reading base rootfs: %w", err)
	}
	baseTr := tar.NewReader(baseGz)
	for {
		hdr, terr := baseTr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			_ = baseGz.Close()
			_ = tw.Close()
			_ = gw.Close()
			_ = f.Close()
			return fmt.Errorf("reading base rootfs tar: %w", terr)
		}
		if terr = tw.WriteHeader(hdr); terr != nil {
			_ = baseGz.Close()
			_ = tw.Close()
			_ = gw.Close()
			_ = f.Close()
			return fmt.Errorf("writing tar header: %w", terr)
		}
		if hdr.Size > 0 {
			if _, terr = io.Copy(tw, baseTr); terr != nil {
				_ = baseGz.Close()
				_ = tw.Close()
				_ = gw.Close()
				_ = f.Close()
				return fmt.Errorf("writing tar data: %w", terr)
			}
		}
	}
	_ = baseGz.Close()

	// Append files from each APK package
	for i, data := range pkgData {
		if terr := appendAPKFiles(tw, data); terr != nil {
			_ = tw.Close()
			_ = gw.Close()
			_ = f.Close()
			return fmt.Errorf("extracting %s: %w", rootfsPackages[i].name, terr)
		}
	}

	if err = tw.Close(); err != nil {
		_ = gw.Close()
		_ = f.Close()
		return fmt.Errorf("closing tar: %w", err)
	}
	if err = gw.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("closing gzip: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("closing output file: %w", err)
	}

	fmt.Printf("  Created %s\n", dest)
	return nil
}

// Golangcilint downloads golangci-lint to ./bin/.
func Golangcilint() error {
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}

	filename := fmt.Sprintf("golangci-lint-%s-%s-%s.%s", GolangCILintVersion, goos, goarch, ext)
	dest := filepath.Join(toolBinDir, "golangci-lint")
	if goos == "windows" {
		dest += ".exe"
	}

	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}

	if err := os.MkdirAll(toolBinDir, 0o755); err != nil {
		return err
	}

	url := fmt.Sprintf("https://github.com/golangci/golangci-lint/releases/download/v%s/%s", GolangCILintVersion, filename)
	fmt.Printf("  Downloading golangci-lint %s...\n", GolangCILintVersion)

	data, err := httpGetBytes(url)
	if err != nil {
		return fmt.Errorf("downloading golangci-lint: %w", err)
	}

	binName := "golangci-lint"
	if goos == "windows" {
		binName = "golangci-lint.exe"
	}
	prefix := fmt.Sprintf("golangci-lint-%s-%s-%s/", GolangCILintVersion, goos, goarch)

	if ext == "tar.gz" {
		gr, gerr := gzip.NewReader(bytes.NewReader(data))
		if gerr != nil {
			return fmt.Errorf("gzip golangci-lint: %w", gerr)
		}
		defer func() { _ = gr.Close() }()

		tr := tar.NewReader(gr)
		for {
			hdr, terr := tr.Next()
			if terr == io.EOF {
				return fmt.Errorf("golangci-lint binary not found in tarball")
			}
			if terr != nil {
				return fmt.Errorf("tar golangci-lint: %w", terr)
			}
			if hdr.Name == prefix+binName {
				f, ferr := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
				if ferr != nil {
					return ferr
				}
				if _, ferr = io.Copy(f, tr); ferr != nil {
					_ = f.Close()
					return ferr
				}
				return f.Close()
			}
		}
	}

	// zip extraction for Windows
	return extractZipBinary(data, prefix+binName, dest)
}

// extractZipBinary extracts a single file from a zip archive in memory.
func extractZipBinary(zipData []byte, entryName, dest string) error {
	r := bytes.NewReader(zipData)
	zr, err := zip.NewReader(r, int64(len(zipData)))
	if err != nil {
		return fmt.Errorf("opening zip: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != entryName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("opening zip entry: %w", err)
		}
		defer func() { _ = rc.Close() }()
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
	return fmt.Errorf("golangci-lint binary not found in zip")
}

// appendAPKFiles extracts data files from an APK package and appends them
// to the tar writer. APK files are concatenated gzip streams: signature,
// control (.PKGINFO etc.), and data (actual files). We process all streams
// but skip metadata entries (names starting with "." but not "./").
func appendAPKFiles(tw *tar.Writer, apkData []byte) error {
	br := bufio.NewReader(bytes.NewReader(apkData))
	gz, err := gzip.NewReader(br)
	if err != nil {
		return fmt.Errorf("reading apk gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)

	for {
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("reading apk tar: %w", err)
			}
			// Skip APK metadata entries (.PKGINFO, .SIGN.*, .pre-install, etc.)
			// Data entries start with "./" (filesystem paths like ./usr/lib/...)
			if strings.HasPrefix(hdr.Name, ".") && !strings.HasPrefix(hdr.Name, "./") {
				continue
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("writing tar header: %w", err)
			}
			if hdr.Size > 0 {
				if _, err := io.Copy(tw, tr); err != nil {
					return fmt.Errorf("writing tar data: %w", err)
				}
			}
		}

		// Advance to next gzip stream; EOF means no more streams
		if err := gz.Reset(br); err != nil {
			break
		}
		gz.Multistream(false)
	}

	return nil
}

// httpGetBytes downloads a URL and returns its body as a byte slice.
func httpGetBytes(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func alpineMajorMinor(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return version
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download shim: HTTP %d", resp.StatusCode)
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip shim: %w", err)
	}
	defer func() { _ = gr.Close() }()

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
				_ = f.Close()
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", dest, resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
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

