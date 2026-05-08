package build

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ephpm/ephemerd/mage/download"
	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

// Install builds ephemerd and installs it to ~/bin, re-signing on macOS.
func Install() error {
	mg.Deps(Build)

	src := "ephemerd"
	if runtime.GOOS == "windows" {
		src = "ephemerd.exe"
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("finding home directory: %w", err)
	}
	destDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", destDir, err)
	}
	dest := filepath.Join(destDir, src)

	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	if err := os.WriteFile(dest, data, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	fmt.Printf("  Installed %s\n", dest)

	if runtime.GOOS == "darwin" {
		return codesign(dest)
	}
	return nil
}

// Build compiles ephemerd for the current OS/arch.
// On Windows this requires the two-stage build (Linux binary + rootfs embed).
func Build() error {
	if runtime.GOOS == "windows" {
		return Windows()
	}
	mg.Deps(download.All)
	output := "ephemerd"
	if env := os.Getenv("OUTPUT"); env != "" {
		output = env
	}
	if err := sh.RunV("go", "build", "-tags", "containers_image_openpgp", "-ldflags", ldflags(), "-o", output, "./cmd/ephemerd/"); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return codesign(output)
	}
	return nil
}

// Windows performs the two-stage Windows build:
// 1. Cross-compile static Linux binary with Linux assets embedded
// 2. Build Windows binary embedding the Linux binary + Alpine rootfs + kernel + initrd
func Windows() error {
	// Phase 1: produce the inputs the initrd will bundle (ephemerd-linux binary
	// and Alpine rootfs tarball), plus downloads that Initrdx86 doesn't touch.
	mg.Deps(Linuxembed, download.Rootfs, download.Runnerwindows, download.Kernelx86, download.Shimwindows)
	// Phase 2: build the initrd. Must run after Linuxembed + Rootfs — Initrdx86
	// bundles pkg/vm/embed/ephemerd-linux and pkg/vm/embed/ephemerd-rootfs-*.tar.gz
	// directly, and will fail on a fresh workspace if those aren't on disk yet.
	mg.Deps(download.Initrdx86)

	// Remove any Linux runner from embed dir to avoid bloating the Windows binary.
	matches, _ := filepath.Glob("pkg/runner/embed/actions-runner-linux-*.tar.gz")
	for _, m := range matches {
		fmt.Printf("  Removing %s (not needed in Windows binary)\n", m)
		_ = os.Remove(m)
	}

	output := "ephemerd.exe"
	if env := os.Getenv("OUTPUT"); env != "" {
		output = env
	}
	return sh.RunWith(
		map[string]string{"CGO_ENABLED": "0", "GOOS": "windows", "GOARCH": "amd64"},
		"go", "build", "-tags", "containers_image_openpgp", "-ldflags", ldflags(), "-o", output, "./cmd/ephemerd/",
	)
}

// Linuxembed cross-compiles a static Linux ephemerd binary for embedding.
func Linuxembed() error {
	mg.Deps(download.Runnerlinux, download.Cnilinux, download.Shimlinux)

	if err := os.MkdirAll("pkg/vm/embed", 0o755); err != nil {
		return err
	}
	return sh.RunWith(
		map[string]string{"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64"},
		"go", "build", "-tags", "containers_image_openpgp", "-ldflags", ldflags(), "-o", "pkg/vm/embed/ephemerd-linux", "./cmd/ephemerd/",
	)
}

// Macos performs the Darwin build with embedded Linux VM assets:
// 1. Download aarch64 rootfs, kernel, and initrd
// 2. Cross-compile static Linux binary (linux/arm64)
// 3. Build macOS binary embedding all VM assets
// 4. Ad-hoc codesign with the virtualization entitlement (Vz requires it)
func Macos() error {
	// The kernel/initrd/rootfs downloads are independent and safe to parallelize.
	mg.Deps(download.Kernel, download.Initrd, download.Rootfs)

	// Linuxembedarm64 temporarily stashes non-Linux runners from pkg/runner/embed
	// while it builds. Runner (downloads osx-arm64) must run after this completes
	// — running them in parallel would let the osx runner re-appear mid-stash and
	// get embedded into ephemerd-linux.
	mg.SerialDeps(Linuxembedarm64, download.Runner)

	// Remove any x86_64 rootfs to avoid embed conflicts
	matches, _ := filepath.Glob("pkg/vm/embed/ephemerd-rootfs-*-x86_64.tar.gz")
	for _, m := range matches {
		fmt.Printf("  Removing %s (not needed in macOS binary)\n", m)
		_ = os.Remove(m)
	}

	output := "ephemerd"
	if env := os.Getenv("OUTPUT"); env != "" {
		output = env
	}

	// The Linux runner is already embedded inside ephemerd-linux (which is itself
	// embedded in the darwin binary). Keeping it in pkg/runner/embed too would
	// triple-embed it. Stash it for the duration of this build.
	stash, err := stashNonMatchingRunners("actions-runner-osx-")
	if err != nil {
		return err
	}
	defer func() { _ = restoreStashed(stash) }()

	if err := sh.RunV("go", "build", "-tags", "containers_image_openpgp", "-ldflags", ldflags(), "-o", output, "./cmd/ephemerd/"); err != nil {
		return err
	}

	return codesign(output)
}

// Linuxembedarm64 cross-compiles a static Linux ephemerd binary for arm64 embedding.
// Temporarily removes non-Linux runners from pkg/runner/embed/ so they don't get
// embedded in the Linux binary (which only needs the Linux runner).
func Linuxembedarm64() error {
	mg.Deps(download.Runnerlinuxarm64, download.Cnilinuxarm64, download.Shimlinuxarm64)

	if err := os.MkdirAll("pkg/vm/embed", 0o755); err != nil {
		return err
	}

	// Move non-Linux runners out of the embed dir for the duration of this build
	// so they don't get triple-embedded via `//go:embed all:embed` in pkg/runner/.
	stash, err := stashNonMatchingRunners("actions-runner-linux-")
	if err != nil {
		return err
	}
	defer func() { _ = restoreStashed(stash) }()

	return sh.RunWith(
		map[string]string{"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "arm64"},
		"go", "build", "-tags", "containers_image_openpgp", "-ldflags", ldflags(), "-o", "pkg/vm/embed/ephemerd-linux", "./cmd/ephemerd/",
	)
}

// stashNonMatchingRunners moves any runner tarball NOT starting with `keepPrefix`
// OUT of the embed directory (to a sibling .stash dir) and returns (original,
// stashed) path pairs for later restoration. Must move OUT of the embed dir —
// `//go:embed all:embed` picks up any file in there regardless of extension.
func stashNonMatchingRunners(keepPrefix string) ([][2]string, error) {
	matches, _ := filepath.Glob("pkg/runner/embed/actions-runner-*")
	stashDir := "pkg/runner/.embed-stash"
	if err := os.MkdirAll(stashDir, 0o755); err != nil {
		return nil, err
	}
	var stashed [][2]string
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.HasPrefix(base, keepPrefix) {
			continue
		}
		stash := filepath.Join(stashDir, base)
		if err := os.Rename(m, stash); err != nil {
			for _, pair := range stashed {
				_ = os.Rename(pair[1], pair[0])
			}
			return nil, fmt.Errorf("stashing %s: %w", m, err)
		}
		stashed = append(stashed, [2]string{m, stash})
	}
	return stashed, nil
}

func restoreStashed(stashed [][2]string) error {
	for _, pair := range stashed {
		if err := os.Rename(pair[1], pair[0]); err != nil {
			return fmt.Errorf("restoring %s: %w", pair[0], err)
		}
	}
	return nil
}

// ForgeRunner compiles the Forgejo Actions runner for the current OS/arch.
func ForgeRunner() error {
	return buildRunner("ephemerd-runner-forgejo")
}

// GiteaRunner compiles the Gitea Actions runner for the current OS/arch.
func GiteaRunner() error {
	return buildRunner("ephemerd-runner-gitea")
}

// Runners compiles both ephemerd-runner-forgejo and ephemerd-runner-gitea for the current OS/arch.
func Runners() {
	mg.Deps(ForgeRunner, GiteaRunner)
}

// RunnersAll cross-compiles ephemerd-runner-forgejo and ephemerd-runner-gitea for all release platforms.
// Outputs go to dist/<name>-<os>-<arch>[.exe].
func RunnersAll() error {
	type target struct{ goos, goarch string }
	targets := []target{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"windows", "amd64"},
		{"darwin", "arm64"},
	}
	for _, t := range targets {
		for _, name := range []string{"ephemerd-runner-forgejo", "ephemerd-runner-gitea"} {
			if err := crossBuildRunner(name, t.goos, t.goarch); err != nil {
				return err
			}
		}
	}
	return nil
}

func buildRunner(name string) error {
	output := name
	if runtime.GOOS == "windows" {
		output += ".exe"
	}
	return sh.RunV("go", "build", "-ldflags", runnerLdflags(), "-o", output, fmt.Sprintf("./cmd/%s/", name))
}

func crossBuildRunner(name, goos, goarch string) error {
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	output := filepath.Join("dist", fmt.Sprintf("%s-%s-%s%s", name, goos, goarch, ext))
	fmt.Printf("  Building %s\n", output)

	if err := os.MkdirAll("dist", 0o755); err != nil {
		return err
	}
	return sh.RunWith(
		map[string]string{"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch},
		"go", "build", "-ldflags", runnerLdflags(), "-o", output, fmt.Sprintf("./cmd/%s/", name),
	)
}

// codesign ad-hoc signs the binary with the virtualization entitlement.
// Set CODESIGN_IDENTITY for a proper Developer ID signature.
func codesign(output string) error {
	identity := "-"
	if env := os.Getenv("CODESIGN_IDENTITY"); env != "" {
		identity = env
	}
	entitlements := filepath.Join("mage", "build", "ephemerd.entitlements")
	fmt.Printf("  Codesigning %s with entitlements (%s)...\n", output, identity)
	return sh.RunV("codesign", "--force", "--sign", identity,
		"--entitlements", entitlements, output)
}

func runnerLdflags() string {
	return fmt.Sprintf("-s -w -X main.version=%s", gitVersion())
}

func ldflags() string {
	version := gitVersion()
	return fmt.Sprintf("-s -w -X main.version=%s -X github.com/ephpm/ephemerd/pkg/runner.Version=%s -X github.com/ephpm/ephemerd/pkg/cni.Version=%s",
		version, download.RunnerVersion, download.CNIVersion)
}

func gitVersion() string {
	out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return "dev"
	}
	return strings.TrimSpace(string(out))
}
