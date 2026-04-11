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
	return sh.RunV("go", "build", "-ldflags", ldflags(), "-o", output, "./cmd/ephemerd/")
}

// Windows performs the two-stage Windows build:
// 1. Cross-compile static Linux binary with Linux assets embedded
// 2. Build Windows binary embedding the Linux binary + Alpine rootfs
func Windows() error {
	mg.Deps(Linuxembed, download.Rootfs, download.Runnerwindows)

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
		map[string]string{"GOOS": "windows", "GOARCH": "amd64"},
		"go", "build", "-ldflags", ldflags(), "-o", output, "./cmd/ephemerd/",
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
		"go", "build", "-ldflags", ldflags(), "-o", "pkg/vm/embed/ephemerd-linux", "./cmd/ephemerd/",
	)
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
