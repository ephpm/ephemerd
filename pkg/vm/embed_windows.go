//go:build windows

package vm

import "embed"

//go:embed embed/ephemerd-linux embed/ephemerd-rootfs-*.tar.gz
var vmFS embed.FS
