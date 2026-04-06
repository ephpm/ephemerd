//go:build windows

package vm

import "embed"

//go:embed embed/ephemerd-linux embed/alpine-minirootfs-*.tar.gz
var vmFS embed.FS
