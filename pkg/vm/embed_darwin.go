//go:build darwin

package vm

import "embed"

//go:embed embed/vmlinuz embed/initrd embed/ephemerd-linux embed/ephemerd-rootfs-*.tar.gz
var vmFS embed.FS
