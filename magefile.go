//go:build mage

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"

	// mage:import build
	build "github.com/ephpm/ephemerd/mage/build"
	// mage:import download
	download "github.com/ephpm/ephemerd/mage/download"
)

// Default target when running mage with no args.
var Default = build.Build

// Test runs all Go tests (downloads embedded deps first if needed).
func Test() error {
	mg.Deps(download.All)
	return sh.RunV("go", "test", "-count=1", "./...")
}

// Lint runs golangci-lint (downloads linter and embedded deps first if needed).
func Lint() error {
	mg.Deps(download.Golangcilint, download.All)
	lint := filepath.Join("bin", "golangci-lint")
	if runtime.GOOS == "windows" {
		lint += ".exe"
	}
	return sh.RunV(lint, "run", "./...")
}

// E2E runs unprivileged e2e tests (tunnel webhook round-trip). Requires GITHUB_TOKEN.
func E2e() error {
	return sh.RunV("go", "test", "-tags", "e2e", "-v", "-timeout", "2m", "./test/e2e/...")
}

// E2EAll runs all e2e tests including privileged ones (requires root + containerd).
func E2eall() error {
	mg.Deps(download.All)
	return sh.RunV("go", "test", "-tags", "e2e,privileged", "-v", "-timeout", "5m", "./test/e2e/...")
}

// E2EForgejo runs the Forgejo provider e2e test.
// Boots a Forgejo instance via docker-compose, runs a full workflow, and tears down.
// Requires: docker with compose support.
func E2eforgejo() error {
	return sh.RunV("go", "test", "-tags", "e2e,privileged", "-v", "-timeout", "3m", "-run", "TestForgejo_E2E", "./test/e2e/forgejo/")
}

// E2EGitea runs the Gitea provider e2e test.
// Boots a Gitea instance via docker-compose, runs a full workflow, and tears down.
// Requires: docker with compose support.
func E2egitea() error {
	return sh.RunV("go", "test", "-tags", "e2e,privileged", "-v", "-timeout", "3m", "-run", "TestGitea_E2E", "./test/e2e/gitea/")
}

// E2EGitLab runs the GitLab CE provider e2e test.
// Boots a GitLab CE instance via docker-compose, runs a full CI pipeline, and tears down.
// Requires: docker with compose support. GitLab CE is resource-heavy (~3GB image, 2-4 min boot).
func E2egitlab() error {
	return sh.RunV("go", "test", "-tags", "e2e,privileged", "-v", "-timeout", "10m", "-run", "TestGitLab_E2E", "./test/e2e/gitlab/")
}

// E2EGitHub runs the GitHub provider e2e test using a fake in-process API server.
// No GitHub account, GITHUB_TOKEN, Docker, or containerd required.
func E2egithub() error {
	return sh.RunV("go", "test", "-tags", "e2e", "-v", "-timeout", "1m", "-run", "TestGitHub_E2E", "./test/e2e/github/")
}

// E2EWoodpecker runs the Woodpecker CI provider e2e test.
// Boots Gitea + Woodpecker Server + Agent via docker-compose, runs a full pipeline, and tears down.
// Requires: docker with compose support.
func E2ewoodpecker() error {
	return sh.RunV("go", "test", "-tags", "e2e,privileged", "-v", "-timeout", "8m", "-run", "TestWoodpecker_E2E", "./test/e2e/woodpecker/")
}

// E2EDind runs the DinD (fake Docker socket) container lifecycle e2e test.
// Boots Forgejo via docker-compose, runs workflows that exercise container
// create/start/inspect via the runner's Docker socket.
// Requires: docker with compose support.
func E2edind() error {
	return sh.RunV("go", "test", "-tags", "e2e,privileged", "-v", "-timeout", "6m", "./test/e2e/dind/")
}

// E2EBuild runs the buildah docker-build e2e tests.
// Tests POST /build through the fake Docker socket against real containerd.
// Requires: root + containerd (privileged).
func E2ebuild() error {
	return sh.RunV("go", "test", "-tags", "linux,e2e,privileged,containers_image_openpgp", "-v", "-timeout", "10m", "-run", "TestE2E_Build", "./test/e2e/")
}

// E2EModProxy runs the Go module proxy e2e test.
// Starts a local proxy, fetches real modules, and builds a small Go app through it.
// Requires: internet access, go toolchain.
func E2emodproxy() error {
	return sh.RunV("go", "test", "-tags", "e2e", "-v", "-timeout", "2m", "-run", "TestModProxy_E2E", "./test/e2e/modproxy/")
}

// E2ERun runs the `ephemerd run` CLI e2e tests.
// Executes `ephemerd run` as a subprocess against test workflows.
// Requires: root + `mage build` (uses embedded containerd).
func E2erun() error {
	return sh.RunV("go", "test", "-tags", "e2e,privileged", "-v", "-timeout", "10m", "-run", "TestE2E_RunCLI", "./test/e2e/")
}

// CI runs download, lint, test, and build.
func CI() {
	mg.SerialDeps(Lint, Test, build.Build)
}

// Generate runs protobuf code generation.
func Generate() error {
	return sh.RunV("protoc",
		"--go_out=.", "--go_opt=paths=source_relative",
		"--go-grpc_out=.", "--go-grpc_opt=paths=source_relative",
		"api/v1/ephemerd.proto",
	)
}

// Clean removes all downloaded assets and build artifacts.
func Clean() error {
	patterns := []string{
		"ephemerd",
		"ephemerd.exe",
		"pkg/runner/embed/actions-runner-*",
		"pkg/cni/embed/cni-plugins-*",
		"pkg/containerd/embed/containerd-shim-runc-v2",
		"pkg/containerd/embed/runc",
		"pkg/vm/embed/ephemerd-linux",
		"pkg/vm/embed/alpine-minirootfs-*",
		"pkg/vm/embed/ephemerd-rootfs-*",
		"pkg/vm/embed/vmlinuz",
		"pkg/vm/embed/initrd",
	}
	for _, p := range patterns {
		matches, _ := filepath.Glob(p)
		for _, m := range matches {
			fmt.Printf("  Removing %s\n", m)
			if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
