package cni

import (
	"fmt"
	"strings"
)

// selectCNITarball picks the CNI plugins archive matching goarch from the
// embed listing. The macOS build cross-compiles ephemerd-linux for arm64 but
// the embed dir can hold BOTH cni-plugins-linux-amd64 and -arm64 (the amd64
// one lingers from the CI cache). `//go:embed all:embed` then bundles both,
// so a naive "first entry" pick returns amd64 and the arm64 Linux VM fails
// every CNI exec with "exec format error". Select by arch instead.
func selectCNITarball(names []string, goarch string) (string, error) {
	want := "cni-plugins-linux-" + goarch + "-"
	var found []string
	for _, name := range names {
		if name == ".gitkeep" {
			continue
		}
		found = append(found, name)
		if strings.HasPrefix(name, want) {
			return name, nil
		}
	}
	if len(found) == 0 {
		return "", fmt.Errorf("no CNI plugins archive found in embedded files (did you run the download targets?)")
	}
	return "", fmt.Errorf("no CNI plugins archive for linux/%s in embedded files (have: %s)", goarch, strings.Join(found, ", "))
}
