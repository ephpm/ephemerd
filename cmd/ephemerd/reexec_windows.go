//go:build windows

package main

// Imported for its init() side effect: registers a "get-user-info" reexec
// handler. BuildKit's Windows containerdexecutor mounts the running
// ephemerd binary into each build container at
// C:\Windows\System32\get-user-info.exe and invokes it with
// argv[0]="get-user-info" to look up Windows user SIDs. Without this
// handler our binary would try to start the full daemon inside the
// container and hang. See moby/buildkit/executor/oci/spec_windows.go.
//
// The package has //go:build windows constraints in its source so it
// can't be imported from main.go directly without breaking the Linux
// cross-compile that produces the embedded VM ephemerd-linux binary.
import _ "github.com/moby/buildkit/util/system/getuserinfo"
