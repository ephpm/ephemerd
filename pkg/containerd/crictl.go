package containerd

import (
	"fmt"
	"os"
	goruntime "runtime"
	"strings"

	"sigs.k8s.io/cri-tools/cmd/crictl"
)

// ExecCrictl runs the crictl CLI in-process against ephemerd's embedded
// containerd. The CRI plugin is registered by the containerd builtins blank
// import (see server.go) and served on the same socket as the native API.
//
// This function sets CONTAINER_RUNTIME_ENDPOINT / IMAGE_SERVICE_ENDPOINT to
// the CRI URI for socketPath, then invokes crictl's urfave/cli v2 app via
// the k3s-io/cri-tools fork's exported Main() entry point (upstream
// cri-tools is package main; the fork patches it to package crictl so we
// can call it as a library). The replace directive is in go.mod.
//
// crictl.Main() may call logrus.Fatal (which os.Exit's) on unrecoverable
// errors. That's acceptable because the caller is a leaf "ephemerd crictl"
// subcommand whose process is meant to exit anyway.
func ExecCrictl(socketPath string, args []string) error {
	endpoint := crictlEndpoint(socketPath)
	if err := os.Setenv("CONTAINER_RUNTIME_ENDPOINT", endpoint); err != nil {
		return fmt.Errorf("setting CONTAINER_RUNTIME_ENDPOINT: %w", err)
	}
	if err := os.Setenv("IMAGE_SERVICE_ENDPOINT", endpoint); err != nil {
		return fmt.Errorf("setting IMAGE_SERVICE_ENDPOINT: %w", err)
	}

	os.Args = append([]string{"crictl"}, args...)
	crictl.Main()
	return nil
}

// crictlEndpoint converts an OS-native socket path into a CRI URI.
// Linux/macOS sockets become unix://; Windows named pipes become npipe://
// using forward-slash form (//./pipe/<name>).
func crictlEndpoint(socketPath string) string {
	if goruntime.GOOS == "windows" {
		p := strings.ReplaceAll(socketPath, `\`, `/`)
		return "npipe://" + p
	}
	return "unix://" + socketPath
}
