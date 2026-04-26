//go:build windows

package buildkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/ephpm/ephemerd/pkg/networking"
	resourcestypes "github.com/moby/buildkit/executor/resources/types"
	"github.com/moby/buildkit/util/network"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// hcnNetworkProvider gives BuildKit's containerd worker a real Windows
// network. BuildKit's stock netproviders.Providers returns a NoneProvider
// on Windows, which leaves the build container with no NetworkAdapter —
// any RUN step that hits the network exits with an immediate failure.
//
// This provider creates a fresh HCN NAT endpoint (and namespace, required
// for runhcs) for each build container via the same pkg/networking.Manager
// the runner uses. Namespace.Set patches the OCI spec's
// Windows.Network.{NetworkNamespace,EndpointList} so runhcs attaches the
// endpoint at container start. Namespace.Close tears the endpoint down.
type hcnNetworkProvider struct {
	netMgr *networking.Manager
	log    *slog.Logger
	seq    atomic.Uint64
}

func newHCNNetworkProvider(netMgr *networking.Manager, log *slog.Logger) network.Provider {
	return &hcnNetworkProvider{netMgr: netMgr, log: log}
}

func (p *hcnNetworkProvider) New(ctx context.Context, hostname string) (network.Namespace, error) {
	id := buildkitEndpointID(hostname, p.seq.Add(1))

	setup, err := p.netMgr.Setup(ctx, id, "")
	if err != nil {
		return nil, fmt.Errorf("buildkit hcn setup for %s: %w", id, err)
	}

	return &hcnNamespace{
		netMgr:     p.netMgr,
		id:         id,
		nsID:       setup.NetNS,
		endpointID: setup.EndpointID,
		log:        p.log,
	}, nil
}

func (p *hcnNetworkProvider) Close() error {
	return nil
}

type hcnNamespace struct {
	netMgr     *networking.Manager
	id         string
	nsID       string
	endpointID string
	log        *slog.Logger
}

func (h *hcnNamespace) Set(s *specs.Spec) error {
	if s.Windows == nil {
		s.Windows = &specs.Windows{}
	}
	if s.Windows.Network == nil {
		s.Windows.Network = &specs.WindowsNetwork{}
	}
	s.Windows.Network.NetworkNamespace = h.nsID
	s.Windows.Network.EndpointList = append(s.Windows.Network.EndpointList, h.endpointID)

	// BuildKit's containerdexecutor on Windows builds the OCI spec from a
	// blank base via populateDefaultWindowsSpec (only sets Cwd=C:\) plus
	// WithEnv(meta.Env). meta.Env carries only Dockerfile ENV/ARG vars from
	// the LLB graph — no PATH, no SystemRoot, no TEMP. Without those the
	// container's PowerShell process can't load .NET TLS providers, hangs
	// silently on Invoke-WebRequest. The runner container path uses
	// oci.WithImageConfig which pulls the base image's PATH; buildkit
	// doesn't apply that. We seed the missing Windows baseline here, only
	// adding values not already present so any user override (e.g.
	// Dockerfile ENV PATH=...) wins.
	if s.Process != nil {
		before := s.Process.Env
		s.Process.Env = ensureWindowsBaselineEnv(s.Process.Env)
		h.log.Info("buildkit container env",
			"before_count", len(before),
			"after_count", len(s.Process.Env),
			"path", findEnv(s.Process.Env, "PATH"),
			"cwd", s.Process.Cwd,
		)
	}
	return nil
}

func findEnv(env []string, key string) string {
	upper := strings.ToUpper(key)
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && strings.ToUpper(k) == upper {
			return v
		}
	}
	return ""
}

// systemPathSegments are the Windows paths a container needs to find
// powershell.exe, cmd.exe, etc. Any LLB op's PATH that doesn't already
// reference C:\Windows\system32 gets these appended.
var systemPathSegments = []string{
	`C:\Windows\system32`,
	`C:\Windows`,
	`C:\Windows\System32\Wbem`,
	`C:\Windows\System32\WindowsPowerShell\v1.0\`,
	`C:\Windows\System32\OpenSSH\`,
}

// ensureWindowsBaselineEnv supplies env vars that buildkit's spec
// generation drops on Windows (it skips WithImageConfig). Two flavors:
//
//   - Vars not present at all → add the default (PATH, SystemRoot, etc).
//   - PATH present but missing system paths → append them. This catches
//     Dockerfile `ENV PATH="C:\go\bin;${PATH}"` constructs where ${PATH}
//     expands to empty (no image-config inheritance) and the resulting
//     PATH lacks System32, breaking subsequent RUN steps that can't even
//     find powershell.exe.
func ensureWindowsBaselineEnv(env []string) []string {
	have := make(map[string]int, len(env))
	for i, e := range env {
		k, _, _ := strings.Cut(e, "=")
		have[strings.ToUpper(k)] = i
	}

	// Smart-merge PATH: if present, ensure it contains system32; otherwise
	// add the full default.
	if i, ok := have["PATH"]; ok {
		_, val, _ := strings.Cut(env[i], "=")
		if !pathContainsSystem32(val) {
			env[i] = "PATH=" + val + ";" + strings.Join(systemPathSegments, ";")
		}
	} else {
		env = append(env, "PATH="+strings.Join(systemPathSegments, ";"))
		have["PATH"] = len(env) - 1
	}

	defaults := []struct{ key, val string }{
		{"SystemRoot", `C:\Windows`},
		{"SystemDrive", `C:`},
		{"USERPROFILE", `C:\Users\ContainerAdministrator`},
		{"USERNAME", `ContainerAdministrator`},
		{"HOMEDRIVE", `C:`},
		{"HOMEPATH", `\Users\ContainerAdministrator`},
		{"TEMP", `C:\Users\ContainerAdministrator\AppData\Local\Temp`},
		{"TMP", `C:\Users\ContainerAdministrator\AppData\Local\Temp`},
		{"PROGRAMFILES", `C:\Program Files`},
		{"ProgramData", `C:\ProgramData`},
		{"PUBLIC", `C:\Users\Public`},
		{"ALLUSERSPROFILE", `C:\ProgramData`},
		{"COMPUTERNAME", `BUILDKITSANDBOX`},
		{"OS", `Windows_NT`},
		{"PATHEXT", `.COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC`},
	}
	for _, d := range defaults {
		if _, ok := have[strings.ToUpper(d.key)]; !ok {
			env = append(env, d.key+"="+d.val)
		}
	}
	return env
}

// pathContainsSystem32 returns true if path looks like it includes the
// Windows system directory in any case form.
func pathContainsSystem32(path string) bool {
	return strings.Contains(strings.ToLower(path), `windows\system32`)
}

func (h *hcnNamespace) Close() error {
	if err := h.netMgr.Teardown(context.Background(), h.id, h.nsID); err != nil {
		h.log.Warn("buildkit hcn teardown", "id", h.id, "error", err)
		return err
	}
	return nil
}

func (h *hcnNamespace) Sample() (*resourcestypes.NetworkSample, error) {
	return nil, nil
}

// buildkitEndpointID returns a containerd/HCN-safe ID for a buildkit
// network endpoint. Falls back to a counter-tagged hostname if random
// fails (it shouldn't, but Setup is in the hot path).
func buildkitEndpointID(hostname string, seq uint64) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("buildkit-%s-%s", hostname, hex.EncodeToString(b[:]))
	}
	return fmt.Sprintf("buildkit-%s-%d", hostname, seq)
}
