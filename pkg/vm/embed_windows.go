//go:build windows

package vm

import "embed"

//go:embed embed/ephemerd-linux embed/ephemerd-rootfs-*.tar.gz embed/vmlinuz embed/initrd
var vmFS embed.FS
