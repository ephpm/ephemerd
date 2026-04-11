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

// Test runs all Go tests.
func Test() error {
	return sh.RunV("go", "test", "./...")
}

// Lint runs golangci-lint (downloads it to ./bin if needed).
func Lint() error {
	mg.Deps(download.GolangCILint)
	lint := filepath.Join("bin", "golangci-lint")
	if runtime.GOOS == "windows" {
		lint += ".exe"
	}
	return sh.RunV(lint, "run", "./...")
}

// CI runs lint and tests.
func CI() {
	mg.SerialDeps(Lint, Test)
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
