//go:build linux

package runtime

import (
	"io/fs"
	"log/slog"
	"os"
	"os/exec"

	"github.com/containerd/containerd/v2/contrib/apparmor"
	"github.com/containerd/containerd/v2/pkg/oci"
)

// hostApparmorResolver is the process-wide resolver for job containers. See
// apparmorResolver for why the decision is re-taken on every create instead of
// being cached for the lifetime of the daemon.
var hostApparmorResolver = newApparmorResolver(apparmorEnv{
	readFile: os.ReadFile,
	stat:     func(name string) (fs.FileInfo, error) { return os.Stat(name) },
	lookPath: exec.LookPath,
	load:     apparmor.LoadDefaultProfile,
})

// apparmorOpts returns the AppArmor confinement for Linux job containers, or
// nil when the host cannot enforce it (AppArmor absent, disabled, securityfs
// unmounted, or the profile only loaded in complain mode).
//
// Loading is done here rather than via apparmor.WithDefaultProfile so that a
// load failure degrades to "unconfined, and say so" instead of erroring out of
// every spec build. log receives one line per change in confinement status.
func apparmorOpts(log *slog.Logger) []oci.SpecOpts {
	name := hostApparmorResolver.profile(log)
	if name == "" {
		return nil
	}
	return []oci.SpecOpts{apparmor.WithProfile(name)}
}
