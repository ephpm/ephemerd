//go:build windows

package vm

import "embed"

//go:embed embed/ephemerd-rootfs-*.tar.gz embed/vmlinuz embed/initrd embed/ephemerd-linux
var vmFS embed.FS
