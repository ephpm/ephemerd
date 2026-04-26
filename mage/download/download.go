package download

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/magefile/mage/mg"
)

const (
	RunnerVersion        = "2.333.1"
	CNIVersion           = "1.6.2"
	ContainerdVersion    = "2.2.2"
	RuncVersion          = "1.3.4"
	AlpineVersion        = "3.21.3"
	LinuxVirtVersion     = "6.12.83-r0"
	GolangCILintVersion  = "2.11.4"

	// Alpine prunes superseded packages from dl-cdn.alpinelinux.org as soon
	// as a new revision lands, so any pinned upstream URL goes 404 within
	// weeks. We mirror linux-virt to a deps/ tag on this repo's GitHub
	// Releases and verify by SHA256 so a re-upload can't silently swap
	// content. Bump procedure: download both APKs from upstream, sha256sum,
	// `gh release create deps/linux-virt-<ver> ...`, upload both, then
	// bump the constants here. See PR #54 for the first such bump.
	LinuxVirtSHA256AArch64 = "7f0bdcf7d16339f90cf7ff44c50b6cabc75e9be877224d6e3076fc5b187c1c65"
	LinuxVirtSHA256X86_64  = "5d06f7833cb19e708a1b74f8cb09f7ddb3eb10e08e9fa716db73287bc7b119c8"

	runnerEmbedDir = "pkg/runner/embed"
	cniEmbedDir    = "pkg/cni/embed"
	shimEmbedDir   = "pkg/containerd/embed"
	vmEmbedDir     = "pkg/vm/embed"
	toolBinDir     = "bin"
)

// All downloads all assets appropriate for the current OS.
func All() {
	switch runtime.GOOS {
	case "darwin":
		mg.Deps(Runner, Kernel, Initrd, Rootfs)
	default:
		mg.Deps(Runner, Cni, Shim)
	}
	// Ensure placeholder files exist for go:embed directives that reference
	// cross-compiled assets (e.g. ephemerd-linux on Windows). Without these,
	// go test/vet/lint fail even though the real files are only needed at runtime.
	_ = EnsurePlaceholders()
}

// EnsurePlaceholders creates empty placeholder files for any go:embed targets
// that don't exist. This allows go test, go vet, and linting to succeed on
// platforms where cross-compiled assets aren't available (e.g. ephemerd-linux
// on Windows without a full two-stage build).
func EnsurePlaceholders() error {
	placeholders := []string{
		filepath.Join(vmEmbedDir, "ephemerd-linux"),
		filepath.Join(vmEmbedDir, "vmlinuz"),
		filepath.Join(vmEmbedDir, "initrd"),
		filepath.Join(vmEmbedDir, "ephemerd-rootfs-placeholder.tar.gz"),
		filepath.Join(shimEmbedDir, "containerd-shim-runc-v2"),
		filepath.Join(shimEmbedDir, "runc"),
		filepath.Join(shimEmbedDir, "containerd-shim-runhcs-v1.exe"),
	}
	for _, p := range placeholders {
		if fileExists(p) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return fmt.Errorf("creating directory for placeholder %s: %w", p, err)
		}
		f, err := os.Create(p)
		if err != nil {
			return fmt.Errorf("creating placeholder %s: %w", p, err)
		}
		_ = f.Close()
	}
	return nil
}

// Runner downloads the GitHub Actions runner archive for the current platform.
func Runner() error {
	os_, arch := runnerPlatform(runtime.GOOS, runtime.GOARCH)
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	filename := fmt.Sprintf("actions-runner-%s-%s-%s.%s", os_, arch, RunnerVersion, ext)
	dest := filepath.Join(runnerEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", RunnerVersion, filename)
	return downloadFile(url, dest)
}

// Runnerlinux always downloads the Linux x64 runner (for embedding in Windows builds).
func Runnerlinux() error {
	filename := fmt.Sprintf("actions-runner-linux-x64-%s.tar.gz", RunnerVersion)
	dest := filepath.Join(runnerEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", RunnerVersion, filename)
	return downloadFile(url, dest)
}

// Runnerlinuxarm64 always downloads the Linux arm64 runner (for macOS embed cross-compile).
func Runnerlinuxarm64() error {
	filename := fmt.Sprintf("actions-runner-linux-arm64-%s.tar.gz", RunnerVersion)
	dest := filepath.Join(runnerEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", RunnerVersion, filename)
	return downloadFile(url, dest)
}

// Runnerwindows always downloads the Windows x64 runner.
func Runnerwindows() error {
	filename := fmt.Sprintf("actions-runner-win-x64-%s.zip", RunnerVersion)
	dest := filepath.Join(runnerEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", RunnerVersion, filename)
	return downloadFile(url, dest)
}

// Cni downloads the CNI plugins tarball (Linux only, no-op on other OS).
func Cni() error {
	if runtime.GOOS != "linux" {
		fmt.Println("Skipping CNI download (not on Linux)")
		return nil
	}
	arch := cniArch(runtime.GOARCH)
	filename := fmt.Sprintf("cni-plugins-linux-%s-v%s.tgz", arch, CNIVersion)
	dest := filepath.Join(cniEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/v%s/%s", CNIVersion, filename)
	return downloadFile(url, dest)
}

// Cnilinux always downloads the Linux amd64 CNI plugins (for cross-compile embed).
func Cnilinux() error {
	filename := fmt.Sprintf("cni-plugins-linux-amd64-v%s.tgz", CNIVersion)
	dest := filepath.Join(cniEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/v%s/%s", CNIVersion, filename)
	return downloadFile(url, dest)
}

// Cnilinuxarm64 downloads the Linux arm64 CNI plugins (for macOS embed cross-compile).
func Cnilinuxarm64() error {
	filename := fmt.Sprintf("cni-plugins-linux-arm64-v%s.tgz", CNIVersion)
	dest := filepath.Join(cniEmbedDir, filename)
	url := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/v%s/%s", CNIVersion, filename)
	return downloadFile(url, dest)
}

// Shim downloads containerd-shim-runc-v2 (extracted from tarball) and runc binary.
func Shim() error {
	if runtime.GOOS != "linux" {
		fmt.Println("Skipping shim download (not on Linux)")
		return nil
	}
	if err := downloadShim(); err != nil {
		return err
	}
	return downloadRunc()
}

// Shimlinux always downloads the Linux amd64 shim + runc (for cross-compile embed).
func Shimlinux() error {
	if err := downloadShimForArch("amd64"); err != nil {
		return err
	}
	return downloadRuncForArch("amd64")
}

// Shimlinuxarm64 downloads the Linux arm64 shim + runc (for macOS embed cross-compile).
func Shimlinuxarm64() error {
	if err := downloadShimForArch("arm64"); err != nil {
		return err
	}
	return downloadRuncForArch("arm64")
}

// Shimwindows builds containerd-shim-runhcs-v1.exe from the hcsshim module.
// This is the Windows container runtime shim that containerd needs to run
// Windows containers (process-isolated or Hyper-V isolated).
func Shimwindows() error {
	dest := filepath.Join(shimEmbedDir, "containerd-shim-runhcs-v1.exe")
	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}

	if err := os.MkdirAll(shimEmbedDir, 0o755); err != nil {
		return fmt.Errorf("creating shim embed dir: %w", err)
	}

	fmt.Println("  Building containerd-shim-runhcs-v1.exe from hcsshim module...")
	cmd := exec.Command("go", "build", "-o", dest, "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1")
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building containerd-shim-runhcs-v1: %w", err)
	}
	return nil
}

// apkPkg describes a package to pre-install in the rootfs.
type apkPkg struct {
	name    string
	version string
	repo    string // "main" or "community"
}

// Packages to pre-install in the rootfs, pinned to Alpine 3.21.3.
// These are the transitive dependencies of gcompat and iptables.
// Update versions when bumping AlpineVersion.
var rootfsPackages = []apkPkg{
	{"musl-obstack", "1.2.3-r2", "main"},
	{"libucontext", "1.3.2-r0", "main"},
	{"gcompat", "1.1.0-r4", "main"},
	{"libmnl", "1.0.5-r2", "main"},
	{"libnftnl", "1.2.8-r0", "main"},
	{"libxtables", "1.8.11-r1", "main"},
	{"iptables", "1.8.11-r1", "main"},
}

// Rootfs builds a custom Alpine rootfs with gcompat and iptables pre-installed.
// Downloads the stock Alpine minirootfs and APK packages from the CDN, then
// combines them into a single tarball — no container runtime required.
// Architecture is determined by runtime.GOARCH.
func Rootfs() error {
	arch := alpineArch(runtime.GOARCH)
	filename := fmt.Sprintf("ephemerd-rootfs-%s-%s.tar.gz", AlpineVersion, arch)
	dest := filepath.Join(vmEmbedDir, filename)

	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}

	if err := os.MkdirAll(vmEmbedDir, 0o755); err != nil {
		return err
	}

	// Remove any old rootfs files to avoid embed conflicts
	for _, pattern := range []string{"alpine-minirootfs-*.tar.gz", "ephemerd-rootfs-*.tar.gz"} {
		oldFiles, _ := filepath.Glob(filepath.Join(vmEmbedDir, pattern))
		for _, f := range oldFiles {
			fmt.Printf("  Removing old rootfs: %s\n", f)
			_ = os.Remove(f)
		}
	}

	baseURL := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/releases/%s/alpine-minirootfs-%s-%s.tar.gz",
		alpineMajorMinor(AlpineVersion), arch, AlpineVersion, arch)
	fmt.Printf("  Downloading base Alpine minirootfs (%s)...\n", arch)
	baseData, err := httpGetBytes(baseURL)
	if err != nil {
		return fmt.Errorf("downloading base rootfs: %w", err)
	}

	pkgData := make([][]byte, len(rootfsPackages))
	for i, pkg := range rootfsPackages {
		url := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/%s/%s/%s-%s.apk",
			alpineMajorMinor(AlpineVersion), pkg.repo, arch, pkg.name, pkg.version)
		fmt.Printf("  Downloading %s-%s.apk (%s)...\n", pkg.name, pkg.version, arch)
		pkgData[i], err = httpGetBytes(url)
		if err != nil {
			return fmt.Errorf("downloading %s: %w", pkg.name, err)
		}
	}

	fmt.Printf("  Building combined rootfs (%s)...\n", arch)
	return buildRootfsTarball(dest, baseData, pkgData, rootfsPackages)
}

// Packages for the initrd (busybox for shell/mount, e2fsprogs for mkfs.ext4).
// These provide the minimal userspace needed to format and mount the root disk.
var initrdPackages = []apkPkg{
	{"musl", "1.2.5-r11", "main"},
	{"busybox-static", "1.37.0-r14", "main"},
	{"e2fsprogs", "1.47.1-r1", "main"},
	{"e2fsprogs-libs", "1.47.1-r1", "main"},
	{"libblkid", "2.40.4-r1", "main"},
	{"libuuid", "2.40.4-r1", "main"},
	{"libcom_err", "1.47.1-r1", "main"},
	{"libeconf", "0.6.3-r0", "main"}, // required by libblkid at runtime
}


// Kernel downloads the Alpine linux-virt kernel and extracts vmlinuz.
// Only needed on Darwin (Virtualization.framework); no-op on other OS.
func Kernel() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	dest := filepath.Join(vmEmbedDir, "vmlinuz")
	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}
	if err := os.MkdirAll(vmEmbedDir, 0o755); err != nil {
		return err
	}

	fmt.Printf("  Downloading linux-virt %s (aarch64) from mirror...\n", LinuxVirtVersion)
	data, err := fetchLinuxVirtAPK("aarch64")
	if err != nil {
		return err
	}

	// Extract the EFI-wrapped vmlinuz-virt from the APK into a temp buffer,
	// then unwrap + decompress to get the raw ARM64 Linux Image that Vz requires.
	tmp := filepath.Join(vmEmbedDir, "vmlinuz.wrapped")
	if err := extractAPKFile(data, "boot/vmlinuz-virt", tmp); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()

	return extractRawArm64Kernel(tmp, dest)
}

// extractRawArm64Kernel reads Alpine's EFI-wrapped "zimg" kernel, finds the
// gzipped payload, decompresses it, and writes the raw ARM64 Linux Image.
// Vz's VZLinuxBootLoader on Apple Silicon requires the uncompressed Image
// (with "ARM\x64" magic at offset 56) — it can't boot EFI-wrapped kernels.
func extractRawArm64Kernel(wrappedPath, dest string) error {
	wrapped, err := os.ReadFile(wrappedPath)
	if err != nil {
		return fmt.Errorf("reading wrapped kernel: %w", err)
	}

	// Alpine's "zimg" header: "MZ\x00\x00" + "zimg" + uint32 payload_offset + uint32 payload_size
	if len(wrapped) < 16 || string(wrapped[4:8]) != "zimg" {
		return fmt.Errorf("kernel is not in Alpine zimg format (header=%q)", wrapped[:8])
	}
	payloadOff := int(wrapped[8]) | int(wrapped[9])<<8 | int(wrapped[10])<<16 | int(wrapped[11])<<24
	if payloadOff <= 0 || payloadOff >= len(wrapped) {
		return fmt.Errorf("invalid zimg payload offset %d (file size %d)", payloadOff, len(wrapped))
	}

	// Decompress gzip payload
	gr, err := gzip.NewReader(bytes.NewReader(wrapped[payloadOff:]))
	if err != nil {
		return fmt.Errorf("opening gzip payload at offset %d: %w", payloadOff, err)
	}
	defer func() { _ = gr.Close() }()
	gr.Multistream(false)

	raw, err := io.ReadAll(gr)
	if err != nil {
		return fmt.Errorf("decompressing kernel: %w", err)
	}
	// Sanity check: ARM64 Linux Image has "ARM\x64" magic at offset 56
	if len(raw) < 64 || string(raw[56:60]) != "ARM\x64" {
		return fmt.Errorf("decompressed payload missing ARM64 Image magic at offset 56")
	}

	fmt.Printf("  Extracted raw ARM64 Image (%d bytes) from zimg wrapper\n", len(raw))
	return os.WriteFile(dest, raw, 0o644)
}

// Kernel modules we need to load early in initrd for Apple Vz compatibility.
// virtio_mmio is the transport driver — Apple Vz exposes virtio devices over
// MMIO (not PCI). Once it's loaded, virtio_blk exposes /dev/vda, virtio_net
// exposes the NAT interface, and virtiofs mounts the host share.
var initrdKernelModules = []string{
	// Order matters: deps must load before users.
	"kernel/drivers/virtio/virtio_mmio.ko.gz",
	"kernel/drivers/block/virtio_blk.ko.gz",
	"kernel/net/core/failover.ko.gz",
	"kernel/drivers/net/net_failover.ko.gz",
	"kernel/drivers/net/virtio_net.ko.gz",
	"kernel/net/packet/af_packet.ko.gz", // required by udhcpc (AF_PACKET raw sockets)
	"kernel/fs/fuse/fuse.ko.gz",
	"kernel/fs/fuse/virtiofs.ko.gz",
	"kernel/lib/crc16.ko.gz",      // ext4 dep
	"kernel/crypto/crc32c_generic.ko.gz", // ext4 dep (for metadata checksums)
	"kernel/lib/libcrc32c.ko.gz",  // ext4 dep
	"kernel/fs/mbcache.ko.gz",     // ext4 dep
	"kernel/fs/jbd2/jbd2.ko.gz",   // ext4 dep
	"kernel/fs/ext4/ext4.ko.gz",
	"kernel/fs/overlayfs/overlay.ko.gz", // containerd overlayfs snapshotter
	// Netfilter + iptables/nftables — needed by CNI bridge + MASQUERADE
	// so containers can reach the internet out of the NAT.
	"kernel/drivers/char/hw_random/virtio-rng.ko.gz", // entropy — without this Go's crypto/rand blocks 60s
	"kernel/net/ipv4/netfilter/nf_defrag_ipv4.ko.gz",
	"kernel/net/netfilter/nf_conntrack.ko.gz",
	"kernel/net/netfilter/nf_nat.ko.gz",
	"kernel/net/netfilter/nfnetlink.ko.gz", // nf_tables transport (required before nf_tables)
	"kernel/net/netfilter/nf_tables.ko.gz",
	"kernel/net/netfilter/nft_chain_nat.ko.gz",
	"kernel/net/netfilter/nft_compat.ko.gz",
	"kernel/net/netfilter/nft_masq.ko.gz",
	"kernel/net/ipv4/netfilter/ip_tables.ko.gz",
	"kernel/net/ipv4/netfilter/iptable_nat.ko.gz",
	"kernel/net/netfilter/xt_MASQUERADE.ko.gz",
	"kernel/net/netfilter/xt_conntrack.ko.gz", // iptables conntrack match extension
	"kernel/net/netfilter/xt_comment.ko.gz",   // CNI uses --comment in NAT rules
	"kernel/net/netfilter/xt_addrtype.ko.gz",  // CNI may use --addrtype matches
	"kernel/net/netfilter/xt_mark.ko.gz",      // common iptables -m mark extension
	"kernel/net/bridge/bridge.ko.gz",          // CNI bridge plugin needs bridge support
	"kernel/drivers/net/veth.ko.gz",           // CNI bridge uses veth pairs to connect containers
	"kernel/net/ipv4/netfilter/ipt_REJECT.ko.gz",   // iptables REJECT target
	"kernel/net/netfilter/nft_reject.ko.gz",         // nf_tables reject support
	"kernel/net/netfilter/nft_reject_inet.ko.gz",    // nf_tables inet reject
}

// Initrd builds a minimal initramfs for the Linux VM on Darwin.
// Contains busybox (static), e2fsprogs (for first-boot mkfs.ext4),
// kernel modules needed for Apple Vz (virtio_mmio and friends), and a
// custom /init script that loads the modules, formats the disk, mounts
// virtio-fs, and exec's ephemerd-linux. No-op on non-Darwin.
func Initrd() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	dest := filepath.Join(vmEmbedDir, "initrd")
	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}
	if err := os.MkdirAll(vmEmbedDir, 0o755); err != nil {
		return err
	}

	// Download APK packages for the initrd userspace
	pkgData := make([][]byte, len(initrdPackages))
	var err error
	for i, pkg := range initrdPackages {
		url := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/%s/aarch64/%s-%s.apk",
			alpineMajorMinor(AlpineVersion), pkg.repo, pkg.name, pkg.version)
		fmt.Printf("  Downloading %s-%s.apk (initrd)...\n", pkg.name, pkg.version)
		pkgData[i], err = httpGetBytes(url)
		if err != nil {
			return fmt.Errorf("downloading %s: %w", pkg.name, err)
		}
	}

	// Download linux-virt APK from our mirror and extract the kernel modules we need.
	fmt.Printf("  Downloading linux-virt %s (aarch64) for kernel modules...\n", LinuxVirtVersion)
	kernelAPK, err := fetchLinuxVirtAPK("aarch64")
	if err != nil {
		return err
	}
	modulePrefix := fmt.Sprintf("lib/modules/%s-0-virt/", linuxKernelReleaseFromVersion(LinuxVirtVersion))

	// First pass: extract modules.dep so we can resolve transitive deps.
	depsFile := modulePrefix + "modules.dep"
	depsRaw, err := extractAPKFilesToMap(kernelAPK, map[string]bool{depsFile: true})
	if err != nil || depsRaw[depsFile] == nil {
		return fmt.Errorf("extracting modules.dep: %w", err)
	}
	deps := parseModulesDep(string(depsRaw[depsFile]))

	// Resolve transitive deps for everything in initrdKernelModules.
	wanted := make(map[string]bool)
	loadOrder := []string{}
	var add func(rel string)
	add = func(rel string) {
		key := modulePrefix + rel
		if wanted[key] {
			return
		}
		// Recurse into deps before adding self so insmod can load in order.
		for _, dep := range deps[rel] {
			add(dep)
		}
		wanted[key] = true
		loadOrder = append(loadOrder, rel)
	}
	for _, m := range initrdKernelModules {
		add(m)
	}

	moduleFiles, err := extractAPKFilesToMap(kernelAPK, wanted)
	if err != nil {
		return fmt.Errorf("extracting kernel modules: %w", err)
	}
	for name := range wanted {
		if _, ok := moduleFiles[name]; !ok {
			return fmt.Errorf("kernel module not found in APK: %s", name)
		}
	}
	fmt.Printf("  Resolved %d initrd kernel modules (with transitive deps)\n", len(loadOrder))

	fmt.Printf("  Building initrd with %d kernel modules...\n", len(moduleFiles))
	return buildInitrd(dest, pkgData, moduleFiles, loadOrder)
}

// Kernel modules for x86_64 Hyper-V initrd.
// Hyper-V synthetic drivers (hv_vmbus, hv_storvsc, hv_netvsc) are built into
// the Alpine linux-virt kernel, not modules — no insmod needed.
// We need: FAT32 for assets disk, ext4 for root disk, overlayfs for containerd,
// netfilter/iptables for CNI MASQUERADE, bridge + veth for container networking.
var initrdKernelModulesX86 = []string{
	// 9P filesystem for host share (Plan9 over VMBus)
	"kernel/net/9p/9pnet.ko.gz",
	"kernel/net/9p/9pnet_virtio.ko.gz",
	"kernel/fs/9p/9p.ko.gz",
	// FAT32 (fallback if 9P unavailable)
	"kernel/fs/fat/fat.ko.gz",
	"kernel/fs/fat/vfat.ko.gz",
	"kernel/fs/nls/nls_cp437.ko.gz",
	"kernel/fs/nls/nls_utf8.ko.gz",
	// af_packet for udhcpc
	"kernel/net/packet/af_packet.ko.gz",
	// ext4 + deps
	"kernel/lib/crc16.ko.gz",
	"kernel/crypto/crc32c_generic.ko.gz",
	"kernel/lib/libcrc32c.ko.gz",
	"kernel/fs/mbcache.ko.gz",
	"kernel/fs/jbd2/jbd2.ko.gz",
	"kernel/fs/ext4/ext4.ko.gz",
	// containerd overlayfs snapshotter
	"kernel/fs/overlayfs/overlay.ko.gz",
	// Netfilter + iptables/nftables for CNI bridge + MASQUERADE
	"kernel/net/ipv4/netfilter/nf_defrag_ipv4.ko.gz",
	"kernel/net/netfilter/nf_conntrack.ko.gz",
	"kernel/net/netfilter/nf_nat.ko.gz",
	"kernel/net/netfilter/nfnetlink.ko.gz",
	"kernel/net/netfilter/nf_tables.ko.gz",
	"kernel/net/netfilter/nft_chain_nat.ko.gz",
	"kernel/net/netfilter/nft_compat.ko.gz",
	"kernel/net/netfilter/nft_masq.ko.gz",
	"kernel/net/ipv4/netfilter/ip_tables.ko.gz",
	"kernel/net/ipv4/netfilter/iptable_nat.ko.gz",
	"kernel/net/netfilter/xt_MASQUERADE.ko.gz",
	"kernel/net/netfilter/xt_conntrack.ko.gz",
	"kernel/net/netfilter/xt_comment.ko.gz",
	"kernel/net/netfilter/xt_addrtype.ko.gz",
	"kernel/net/netfilter/xt_mark.ko.gz",
	"kernel/net/bridge/bridge.ko.gz",
	"kernel/drivers/net/veth.ko.gz",
	// Hyper-V utilities for time sync (TimeSync IC)
	"kernel/drivers/hv/hv_utils.ko.gz",
	// Hyper-V synthetic SCSI storage driver + SCSI disk. Required so the
	// attached VHDX shows up as /dev/sda. Not builtin in Alpine's linux-virt;
	// transitive deps (scsi_mod, scsi_common, etc.) are pulled in by
	// modules.dep resolution.
	"kernel/drivers/scsi/hv_storvsc.ko.gz",
	"kernel/drivers/scsi/sd_mod.ko.gz",
}

// Kernelx86 downloads the Alpine linux-virt kernel for x86_64 and extracts
// the uncompressed vmlinux from the bzImage. HCS LinuxKernelDirect requires
// an uncompressed ELF kernel, not the compressed bzImage.
func Kernelx86() error {
	dest := filepath.Join(vmEmbedDir, "vmlinuz")
	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}
	if err := os.MkdirAll(vmEmbedDir, 0o755); err != nil {
		return err
	}

	fmt.Printf("  Downloading linux-virt %s (x86_64) from mirror...\n", LinuxVirtVersion)
	data, err := fetchLinuxVirtAPK("x86_64")
	if err != nil {
		return err
	}

	// Extract the bzImage from the APK to a temp file
	tmp := filepath.Join(vmEmbedDir, "vmlinuz.bzimage")
	if err := extractAPKFile(data, "boot/vmlinuz-virt", tmp); err != nil {
		return fmt.Errorf("extracting vmlinuz: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(tmp); removeErr != nil {
			fmt.Printf("  warning: could not remove temp bzImage: %v\n", removeErr)
		}
	}()

	// Decompress the bzImage to get the raw ELF vmlinux.
	// HCS LinuxKernelDirect needs the uncompressed kernel.
	if err := extractVmlinuxFromBzImage(tmp, dest); err != nil {
		return err
	}

	st, err := os.Stat(dest)
	if err != nil {
		return err
	}
	fmt.Printf("  Extracted uncompressed x86_64 vmlinux (%d bytes)\n", st.Size())
	return nil
}

// extractVmlinuxFromBzImage finds the gzip-compressed payload inside an
// x86_64 bzImage and decompresses it to get the raw ELF vmlinux.
// The bzImage format embeds a compressed kernel that can be found by
// scanning for the gzip magic bytes (0x1f 0x8b 0x08).
func extractVmlinuxFromBzImage(bzImagePath, dest string) error {
	data, err := os.ReadFile(bzImagePath)
	if err != nil {
		return fmt.Errorf("reading bzImage: %w", err)
	}

	// Scan for gzip magic bytes. The first occurrence is typically the
	// compressed kernel payload (may also match compressed data tables,
	// so we try each gzip stream until we find one containing an ELF binary).
	magic := []byte{0x1f, 0x8b, 0x08}
	for i := 0; i <= len(data)-3; i++ {
		if data[i] != magic[0] || data[i+1] != magic[1] || data[i+2] != magic[2] {
			continue
		}

		gr, gzErr := gzip.NewReader(bytes.NewReader(data[i:]))
		if gzErr != nil {
			continue
		}
		gr.Multistream(false)

		raw, readErr := io.ReadAll(gr)
		if closeErr := gr.Close(); closeErr != nil {
			fmt.Printf("  warning: gzip close error at offset %d: %v\n", i, closeErr)
		}
		if readErr != nil || len(raw) < 64 {
			continue
		}

		// Check for ELF magic: 0x7f 'E' 'L' 'F'
		if raw[0] == 0x7f && raw[1] == 'E' && raw[2] == 'L' && raw[3] == 'F' {
			fmt.Printf("  Found ELF vmlinux at bzImage offset %d (%d bytes)\n", i, len(raw))
			return os.WriteFile(dest, raw, 0o644)
		}
	}

	return fmt.Errorf("no gzip-compressed ELF payload found in bzImage")
}

// Initrdx86 builds a minimal initramfs for the Linux VM on Windows (Hyper-V).
// Contains busybox (static), e2fsprogs (for first-boot mkfs.ext4),
// kernel modules needed for Hyper-V (FAT32, ext4, netfilter), and a custom
// /init script that loads modules, mounts the root + assets disks, and
// exec's ephemerd-linux as PID 1.
func Initrdx86() error {
	dest := filepath.Join(vmEmbedDir, "initrd")
	// Initrd embeds ephemerd-linux, the rootfs tarball, and the kernel
	// modules from linux-virt. If any of those is newer than the existing
	// initrd, the embedded copy is stale and we must rebuild — otherwise a
	// fresh ephemerd-linux silently runs an old initrd's binary copy.
	inputs := []string{
		filepath.Join(vmEmbedDir, "ephemerd-linux"),
		filepath.Join(vmEmbedDir, "ephemerd-rootfs-"+AlpineVersion+"-x86_64.tar.gz"),
	}
	if !outOfDate(dest, inputs...) {
		fmt.Printf("  %s already up to date, skipping\n", dest)
		return nil
	}
	if err := os.MkdirAll(vmEmbedDir, 0o755); err != nil {
		return err
	}

	// Download APK packages for the initrd userspace (same as Darwin, x86_64 arch)
	pkgData := make([][]byte, len(initrdPackages))
	var err error
	for i, pkg := range initrdPackages {
		url := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/%s/x86_64/%s-%s.apk",
			alpineMajorMinor(AlpineVersion), pkg.repo, pkg.name, pkg.version)
		fmt.Printf("  Downloading %s-%s.apk (initrd x86_64)...\n", pkg.name, pkg.version)
		pkgData[i], err = httpGetBytes(url)
		if err != nil {
			return fmt.Errorf("downloading %s: %w", pkg.name, err)
		}
	}

	// Download linux-virt APK for x86_64 from our mirror and extract kernel modules.
	fmt.Printf("  Downloading linux-virt %s (x86_64) for kernel modules...\n", LinuxVirtVersion)
	kernelAPK, err := fetchLinuxVirtAPK("x86_64")
	if err != nil {
		return err
	}
	modulePrefix := fmt.Sprintf("lib/modules/%s-0-virt/", linuxKernelReleaseFromVersion(LinuxVirtVersion))

	// Extract modules.dep for transitive dependency resolution
	depsFile := modulePrefix + "modules.dep"
	depsRaw, err := extractAPKFilesToMap(kernelAPK, map[string]bool{depsFile: true})
	if err != nil || depsRaw[depsFile] == nil {
		return fmt.Errorf("extracting modules.dep: %w", err)
	}
	deps := parseModulesDep(string(depsRaw[depsFile]))

	// Resolve transitive deps for everything in initrdKernelModulesX86
	wanted := make(map[string]bool)
	loadOrder := []string{}
	var add func(rel string)
	add = func(rel string) {
		key := modulePrefix + rel
		if wanted[key] {
			return
		}
		for _, dep := range deps[rel] {
			add(dep)
		}
		wanted[key] = true
		loadOrder = append(loadOrder, rel)
	}
	for _, m := range initrdKernelModulesX86 {
		add(m)
	}

	moduleFiles, err := extractAPKFilesToMap(kernelAPK, wanted)
	if err != nil {
		return fmt.Errorf("extracting kernel modules: %w", err)
	}
	for name := range wanted {
		if _, ok := moduleFiles[name]; !ok {
			return fmt.Errorf("kernel module not found in APK: %s", name)
		}
	}
	fmt.Printf("  Resolved %d initrd kernel modules for x86_64 (with transitive deps)\n", len(loadOrder))

	fmt.Printf("  Building x86_64 initrd with %d kernel modules...\n", len(moduleFiles))
	return buildInitrdX86(dest, pkgData, moduleFiles, loadOrder)
}

// linuxKernelReleaseFromVersion converts "6.12.81-r0" → "6.12.81".
// Alpine's APK version is "<kernel-release>-r<revision>".
func linuxKernelReleaseFromVersion(v string) string {
	if i := strings.Index(v, "-r"); i > 0 {
		return v[:i]
	}
	return v
}

// parseModulesDep parses kernel `modules.dep` into a map of relative
// module path → list of relative dep paths. Lines look like:
//
//	kernel/foo/bar.ko.gz: kernel/baz.ko.gz kernel/qux.ko.gz
func parseModulesDep(content string) map[string][]string {
	out := make(map[string][]string)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		mod := strings.TrimSpace(line[:colon])
		rest := strings.TrimSpace(line[colon+1:])
		if rest == "" {
			out[mod] = nil
			continue
		}
		out[mod] = strings.Fields(rest)
	}
	return out
}

// extractAPKFilesToMap extracts a specific set of files from an APK and
// returns their contents keyed by path.
func extractAPKFilesToMap(apkData []byte, wanted map[string]bool) (map[string][]byte, error) {
	out := make(map[string][]byte, len(wanted))
	br := bufio.NewReader(bytes.NewReader(apkData))
	gz, err := gzip.NewReader(br)
	if err != nil {
		return nil, fmt.Errorf("reading apk gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)

	for {
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("reading apk tar: %w", err)
			}
			name := strings.TrimPrefix(hdr.Name, "./")
			if !wanted[name] {
				continue
			}
			data, rerr := io.ReadAll(tr)
			if rerr != nil {
				return nil, fmt.Errorf("reading %s: %w", name, rerr)
			}
			out[name] = data
		}
		if err := gz.Reset(br); err != nil {
			break
		}
		gz.Multistream(false)
	}
	return out, nil
}

// buildInitrd creates a gzip-compressed cpio archive containing a minimal
// initramfs with busybox, e2fsprogs, kernel modules, and a custom /init script.
func buildInitrd(dest string, pkgData [][]byte, moduleFiles map[string][]byte, loadOrder []string) error {
	// First pass: extract all APK files into a temporary tar to collect them
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i, data := range pkgData {
		if err := appendAPKFiles(tw, data); err != nil {
			return fmt.Errorf("extracting %s: %w", initrdPackages[i].name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("closing tar: %w", err)
	}

	// Collect files from the tar into a map for cpio writing
	type cpioFile struct {
		name string
		mode int64
		size int64
		data []byte
		link string // symlink target
	}
	var files []cpioFile
	seen := make(map[string]bool)

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading collected tar: %w", err)
		}
		// Normalize: strip leading "./"
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "" || name == "." {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true

		cf := cpioFile{name: name, mode: int64(hdr.Mode)}
		switch hdr.Typeflag {
		case tar.TypeDir:
			cf.mode |= 0o40000 // directory
			files = append(files, cf)
		case tar.TypeSymlink:
			cf.link = hdr.Linkname
			cf.mode |= 0o120000 // symlink
			files = append(files, cf)
		case tar.TypeReg:
			data, rerr := io.ReadAll(tr)
			if rerr != nil {
				return fmt.Errorf("reading %s: %w", name, rerr)
			}
			cf.size = int64(len(data))
			cf.data = data
			cf.mode |= 0o100000 // regular file
			files = append(files, cf)
		}
	}

	// Add required initrd directories
	for _, dir := range []string{"dev", "proc", "sys", "mnt", "mnt/root", "mnt/ephemerd", "newroot", "tmp"} {
		if !seen[dir] {
			files = append(files, cpioFile{name: dir, mode: 0o40755})
			seen[dir] = true
		}
	}

	// Add busybox symlinks for common applets
	busyboxPath := "bin/busybox.static"
	if !seen[busyboxPath] {
		busyboxPath = "bin/busybox"
	}
	for _, applet := range []string{
		"bin/sh", "bin/mount", "bin/umount", "bin/mkdir", "bin/cat",
		"bin/echo", "bin/sleep", "bin/ls", "bin/cp", "bin/mv", "bin/rm",
		"bin/ln", "bin/grep", "bin/sed", "bin/tar", "bin/gzip", "bin/gunzip",
		"bin/head", "bin/tail", "bin/wc", "bin/find", "bin/stat", "bin/chmod",
		"bin/test", "bin/true", "bin/false", "bin/date", "bin/dmesg",
		"bin/ip", "bin/ping", "bin/hostname", "bin/dd",
		"sbin/switch_root", "sbin/modprobe", "sbin/mdev", "sbin/insmod",
		"sbin/udhcpc", "sbin/ifconfig", "sbin/route",
		"sbin/mkswap", "sbin/swapon", "sbin/swapoff",
	} {
		if !seen[applet] {
			dir := applet[:strings.LastIndex(applet, "/")]
			if !seen[dir] {
				files = append(files, cpioFile{name: dir, mode: 0o40755})
				seen[dir] = true
			}
			// Relative symlink from e.g. bin/sh -> busybox.static
			target := busyboxPath[strings.LastIndex(busyboxPath, "/")+1:]
			if dir != "bin" {
				target = "../" + busyboxPath
			}
			files = append(files, cpioFile{name: applet, mode: 0o120777, link: target})
			seen[applet] = true
		}
	}

	// Add udhcpc handler script. Busybox's udhcpc calls out to this on
	// each lease event — without it the IP lease is never applied.
	udhcpcScript := `#!/bin/sh
# Minimal udhcpc handler for the Vz NAT lease.
case "$1" in
    deconfig)
        ip addr flush dev "$interface" 2>/dev/null
        ;;
    bound|renew)
        ip addr flush dev "$interface" 2>/dev/null
        if [ -n "$subnet" ]; then
            ip addr add "$ip/$subnet" dev "$interface"
        else
            ip addr add "$ip/24" dev "$interface"
        fi
        ip link set "$interface" up
        if [ -n "$router" ]; then
            ip route del default 2>/dev/null
            ip route add default via "$router" dev "$interface"
        fi
        if [ -n "$dns" ]; then
            echo "# generated by udhcpc" > /etc/resolv.conf
            for d in $dns; do echo "nameserver $d" >> /etc/resolv.conf; done
        fi
        ;;
esac
exit 0
`
	for _, dir := range []string{"usr", "usr/share", "usr/share/udhcpc", "etc"} {
		if !seen[dir] {
			files = append(files, cpioFile{name: dir, mode: 0o40755})
			seen[dir] = true
		}
	}
	files = append(files, cpioFile{
		name: "usr/share/udhcpc/default.script",
		mode: 0o100755,
		size: int64(len(udhcpcScript)),
		data: []byte(udhcpcScript),
	})

	// Add kernel modules at /modules/<basename>.ko.gz (flat layout — the
	// init script insmods them by basename). Using a flat dir keeps the
	// init script simple and avoids depending on a specific kernel release.
	if !seen["modules"] {
		files = append(files, cpioFile{name: "modules", mode: 0o40755})
		seen["modules"] = true
	}
	for modPath, modData := range moduleFiles {
		base := modPath[strings.LastIndex(modPath, "/")+1:]
		name := "modules/" + base
		if seen[name] {
			continue
		}
		files = append(files, cpioFile{
			name: name,
			mode: 0o100644,
			size: int64(len(modData)),
			data: modData,
		})
		seen[name] = true
	}

	// Add /init script
	initScript := `#!/bin/sh
# ephemerd initrd init script
# Loads virtio-mmio kernel modules, mounts root disk, optionally formats,
# mounts virtio-fs, pivots, and exec's ephemerd-linux as PID 1.

mount -t proc     none /proc
mount -t sysfs    none /sys
mount -t devtmpfs none /dev

# Load virtio-mmio transport first — on Apple Vz, all virtio devices are
# MMIO-attached. Without virtio_mmio.ko there's no /dev/vda, no network, etc.
# Load order matters: transport → bus consumers → filesystems.
echo "ephemerd-init: loading virtio kernel modules"
for mod in __MODULE_LOAD_ORDER__; do
    if [ -f /modules/${mod}.ko.gz ]; then
        gunzip -c /modules/${mod}.ko.gz > /tmp/${mod}.ko
        insmod /tmp/${mod}.ko || echo "ephemerd-init: insmod ${mod} failed (may already be loaded)"
        rm -f /tmp/${mod}.ko
    fi
done

# Give udev/devtmpfs a moment to populate /dev after modules attach
sleep 1

# Parse kernel command line
CONTAINERD_PORT="10000"
SHARE_TAG="ephemerd"
for param in $(cat /proc/cmdline); do
    case "$param" in
        ephemerd.containerd_port=*) CONTAINERD_PORT="${param#*=}" ;;
        ephemerd.share_tag=*)       SHARE_TAG="${param#*=}" ;;
    esac
done

echo "ephemerd-init: containerd_port=$CONTAINERD_PORT share_tag=$SHARE_TAG"
ls /dev/vd* 2>/dev/null && echo "ephemerd-init: block devices found" || echo "ephemerd-init: WARNING: no /dev/vd* devices"

# Bring up the NAT interface and acquire an IP via DHCP so the host can
# reach us at our Vz-assigned address (Apple Vz NAT: 192.168.64.0/24).
NET_IF=""
for iface in eth0 ens3 enp0s3; do
    if [ -d /sys/class/net/$iface ]; then
        NET_IF=$iface
        break
    fi
done
if [ -n "$NET_IF" ]; then
    echo "ephemerd-init: bringing up $NET_IF"
    ip link set lo up
    ip link set "$NET_IF" up
    # Run udhcpc verbose so failures surface. -n = exit after obtaining lease
    # (or exhausting retries). -t 8 = 8 DISCOVER attempts.
    echo "ephemerd-init: running udhcpc on $NET_IF"
    udhcpc -i "$NET_IF" -n -t 8 -s /usr/share/udhcpc/default.script
    UDHCPC_RC=$?
    echo "ephemerd-init: udhcpc exit=$UDHCPC_RC"
    echo "ephemerd-init: $NET_IF state:"
    ip -4 addr show "$NET_IF"
    echo "ephemerd-init: routes:"
    ip route
    # Ping the Vz NAT gateway (always 192.168.64.1) to populate the host's
    # ARP table so it can look us up by MAC. Without this the host never
    # sees our MAC → IP mapping and our waitForContainerd times out.
    echo "ephemerd-init: pinging 192.168.64.1 to populate host ARP"
    ping -c 2 -W 1 192.168.64.1 || echo "ephemerd-init: WARNING: gateway ping failed"
else
    echo "ephemerd-init: WARNING: no NAT network interface found"
fi

# Mount root disk, formatting on first boot
if ! mount -t ext4 /dev/vda /newroot 2>/dev/null; then
    echo "ephemerd-init: first boot, formatting /dev/vda as ext4"
    /sbin/mkfs.ext4 -q -L ephemerd /dev/vda || {
        echo "ephemerd-init: FATAL: mkfs.ext4 failed"
        exec /bin/sh
    }
    mount -t ext4 /dev/vda /newroot || {
        echo "ephemerd-init: FATAL: mount after mkfs failed"
        exec /bin/sh
    }
fi

# Extract rootfs on first boot (no /sbin/init on the disk)
if [ ! -x /newroot/sbin/openrc-init ] && [ ! -x /newroot/bin/busybox ]; then
    echo "ephemerd-init: extracting rootfs to /dev/vda"
    mkdir -p /mnt/ephemerd
    if mount -t virtiofs "$SHARE_TAG" /mnt/ephemerd 2>/dev/null; then
        ROOTFS=""
        for f in /mnt/ephemerd/vm/linux/ephemerd-rootfs-*.tar.gz; do
            [ -f "$f" ] && ROOTFS="$f" && break
        done
        if [ -n "$ROOTFS" ]; then
            tar xzf "$ROOTFS" -C /newroot
            echo "ephemerd-init: rootfs extracted from $ROOTFS"
        else
            echo "ephemerd-init: FATAL: no rootfs tarball found in share"
            umount /mnt/ephemerd
            exec /bin/sh
        fi
        umount /mnt/ephemerd
    else
        echo "ephemerd-init: FATAL: could not mount virtio-fs share"
        exec /bin/sh
    fi
fi

# Make sure mountpoints exist in the new root
mkdir -p /newroot/proc /newroot/sys /newroot/dev /newroot/mnt/ephemerd /newroot/tmp /newroot/var/lib/ephemerd

# Move kernel filesystems into newroot so ephemerd-linux can see them
mount --move /proc /newroot/proc
mount --move /sys  /newroot/sys
mount --move /dev  /newroot/dev

# cgroup v2 — required by runc/containerd. Mount in the newroot so it's
# visible to ephemerd-linux (and therefore to containerd and runc) after
# switch_root. Without this runc fails with "no cgroup mount found".
mkdir -p /newroot/sys/fs/cgroup
mount -t cgroup2 none /newroot/sys/fs/cgroup || \
    echo "ephemerd-init: WARNING: cgroup2 mount failed"

# Mount virtio-fs share at the final location
mount -t virtiofs "$SHARE_TAG" /newroot/mnt/ephemerd || \
    echo "ephemerd-init: WARNING: could not mount virtio-fs share"

# DNS config — the Vz NAT gateway forwards DNS to the host's resolvers.
# DHCP sometimes returns ::1 (IPv6 loopback) which has nothing listening.
GW=$(ip route | sed -n 's/^default via \([^ ]*\).*/\1/p')
if [ -n "$GW" ]; then
    echo "nameserver $GW" > /newroot/etc/resolv.conf
    echo "ephemerd-init: DNS set to gateway $GW"
else
    echo "ephemerd-init: WARNING: no default gateway, DNS may not work"
fi

# Pivot to real root and exec ephemerd-linux as PID 1.
# The binary lives in the host's data dir, accessed via the virtio-fs share.
EPHEMERD_BIN=/mnt/ephemerd/vm/linux/ephemerd-linux
if [ ! -x "/newroot${EPHEMERD_BIN}" ]; then
    echo "ephemerd-init: FATAL: ${EPHEMERD_BIN} not found or not executable"
    exec /bin/sh
fi

# Set up a sane PATH — ephemerd-linux may shell out to containerd helpers.
export PATH=/usr/bin:/usr/sbin:/bin:/sbin
export HOME=/root

echo "ephemerd-init: pivoting to real root and launching ephemerd-linux"
exec switch_root /newroot "$EPHEMERD_BIN" serve \
    --data-dir /var/lib/ephemerd \
    --containerd-tcp-port "$CONTAINERD_PORT" \
    --containerd-tcp-addr 0.0.0.0 \
    --containerd-only
`
	// Substitute the resolved module load order. Strip the `kernel/.../`
	// directory and `.ko.gz` suffix to get the basename insmod expects.
	modNames := make([]string, 0, len(loadOrder))
	for _, m := range loadOrder {
		base := m[strings.LastIndex(m, "/")+1:]
		base = strings.TrimSuffix(base, ".ko.gz")
		modNames = append(modNames, base)
	}
	initScript = strings.Replace(initScript, "__MODULE_LOAD_ORDER__", strings.Join(modNames, " "), 1)

	files = append(files, cpioFile{
		name: "init",
		mode: 0o100755,
		size: int64(len(initScript)),
		data: []byte(initScript),
	})

	// Write cpio archive (newc format) compressed with gzip
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating initrd: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dest)
		}
	}()

	gw := gzip.NewWriter(f)
	for _, cf := range files {
		if err := writeCPIOEntry(gw, cf.name, cf.mode, cf.data, cf.link); err != nil {
			_ = gw.Close()
			_ = f.Close()
			return fmt.Errorf("writing cpio entry %s: %w", cf.name, err)
		}
	}
	// Write TRAILER
	if err := writeCPIOEntry(gw, "TRAILER!!!", 0, nil, ""); err != nil {
		_ = gw.Close()
		_ = f.Close()
		return fmt.Errorf("writing cpio trailer: %w", err)
	}
	// Pad to 512-byte boundary
	if err := gw.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("closing gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing initrd file: %w", err)
	}

	fmt.Printf("  Created %s\n", dest)
	return nil
}

// buildInitrdX86 builds the x86_64 initrd for Hyper-V. Same CPIO structure as
// buildInitrd but with a Hyper-V-specific init script that uses SCSI devices
// (/dev/sda root, /dev/sdb1 FAT32 assets) instead of virtio devices.
func buildInitrdX86(dest string, pkgData [][]byte, moduleFiles map[string][]byte, loadOrder []string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i, data := range pkgData {
		if err := appendAPKFiles(tw, data); err != nil {
			return fmt.Errorf("extracting %s: %w", initrdPackages[i].name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("closing tar: %w", err)
	}

	// Convert the collected tar entries into cpio entries
	type cpioFile struct {
		name string
		mode int64
		size int64
		data []byte
		link string
	}
	var files []cpioFile
	seen := make(map[string]bool)

	// Standard dirs
	for _, d := range []string{"dev", "proc", "sys", "tmp", "newroot", "mnt", "mnt/assets", "modules",
		"usr", "usr/bin", "usr/sbin", "usr/lib", "usr/share", "usr/share/udhcpc",
		"bin", "sbin", "lib", "etc", "var", "var/lib", "var/lib/ephemerd"} {
		files = append(files, cpioFile{name: d, mode: 0o40755})
		seen[d] = true
	}

	// Extract APK files from the tar
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		name = strings.TrimPrefix(name, "/")
		if name == "" || name == "." || strings.HasPrefix(name, ".PKGINFO") || strings.HasPrefix(name, ".SIGN") {
			continue
		}

		if seen[name] {
			continue
		}
		seen[name] = true

		switch hdr.Typeflag {
		case tar.TypeDir:
			files = append(files, cpioFile{name: name, mode: int64(hdr.Mode) | 0o40000})
		case tar.TypeSymlink:
			files = append(files, cpioFile{name: name, mode: 0o120777, link: hdr.Linkname})
		case tar.TypeReg:
			data, readErr := io.ReadAll(tr)
			if readErr != nil {
				return fmt.Errorf("reading %s: %w", name, readErr)
			}
			files = append(files, cpioFile{name: name, mode: int64(hdr.Mode) | 0o100000, size: int64(len(data)), data: data})
		}
	}

	// Add kernel modules under /modules/ (flat directory, basename only)
	for fullPath, modData := range moduleFiles {
		name := "modules/" + fullPath[strings.LastIndex(fullPath, "/")+1:]
		files = append(files, cpioFile{
			name: name,
			mode: 0o100644,
			size: int64(len(modData)),
			data: modData,
		})
		seen[name] = true
	}

	// Add busybox symlinks for common applets (same as Darwin initrd)
	busyboxPath := "bin/busybox.static"
	if !seen[busyboxPath] {
		busyboxPath = "bin/busybox"
	}
	for _, applet := range []string{
		"bin/sh", "bin/mount", "bin/umount", "bin/mkdir", "bin/cat",
		"bin/echo", "bin/sleep", "bin/ls", "bin/cp", "bin/mv", "bin/rm",
		"bin/ln", "bin/grep", "bin/sed", "bin/tar", "bin/gzip", "bin/gunzip",
		"bin/head", "bin/tail", "bin/wc", "bin/find", "bin/stat", "bin/chmod",
		"bin/test", "bin/true", "bin/false", "bin/date", "bin/dmesg",
		"bin/ip", "bin/ping", "bin/hostname", "bin/dd",
		"sbin/switch_root", "sbin/modprobe", "sbin/mdev", "sbin/insmod",
		"sbin/udhcpc", "sbin/ifconfig", "sbin/route",
		"sbin/mkswap", "sbin/swapon", "sbin/swapoff",
	} {
		if !seen[applet] {
			dir := applet[:strings.LastIndex(applet, "/")]
			if !seen[dir] {
				files = append(files, cpioFile{name: dir, mode: 0o40755})
				seen[dir] = true
			}
			target := busyboxPath[strings.LastIndex(busyboxPath, "/")+1:]
			if dir != "bin" {
				target = "../" + busyboxPath
			}
			files = append(files, cpioFile{name: applet, mode: 0o120777, link: target})
			seen[applet] = true
		}
	}

	// Add udhcpc handler script (same as Darwin initrd)
	udhcpcScript := "#!/bin/sh\ncase $1 in\n  bound|renew)\n    ip addr flush dev $interface\n    ip addr add $ip/$mask dev $interface\n    [ -n \"$router\" ] && ip route add default via $router dev $interface\n    [ -n \"$dns\" ] && echo \"nameserver $dns\" > /etc/resolv.conf\n    ;;\nesac\n"
	udhcpcDir := "usr/share/udhcpc"
	if !seen[udhcpcDir] {
		files = append(files, cpioFile{name: udhcpcDir, mode: 0o40755})
		seen[udhcpcDir] = true
	}
	files = append(files, cpioFile{
		name: "usr/share/udhcpc/default.script",
		mode: 0o100755,
		size: int64(len(udhcpcScript)),
		data: []byte(udhcpcScript),
	})

	// Bundle rootfs tarball and ephemerd-linux binary into the initrd at /assets/.
	// The init script extracts these to tmpfs at boot — no disk attachment needed.
	files = append(files, cpioFile{name: "assets", mode: 0o40755})

	// Read rootfs and ephemerd-linux from the embed directory on disk.
	// These files are placed there by download.Rootfs and build.Linuxembed.
	rootfsMatches, _ := filepath.Glob(filepath.Join(vmEmbedDir, "ephemerd-rootfs-*.tar.gz"))
	if len(rootfsMatches) == 0 {
		return fmt.Errorf("no rootfs tarball found in %s (run 'mage download:rootfs' first)", vmEmbedDir)
	}
	rootfsData, err := os.ReadFile(rootfsMatches[0])
	if err != nil {
		return fmt.Errorf("reading rootfs for initrd bundle: %w", err)
	}
	files = append(files, cpioFile{
		name: "assets/rootfs.tar.gz",
		mode: 0o100644,
		size: int64(len(rootfsData)),
		data: rootfsData,
	})
	fmt.Printf("  Bundling rootfs in initrd (%d bytes)\n", len(rootfsData))

	ephemerdPath := filepath.Join(vmEmbedDir, "ephemerd-linux")
	ephemerdData, err := os.ReadFile(ephemerdPath)
	if err != nil {
		return fmt.Errorf("reading ephemerd-linux for initrd bundle: %w", err)
	}
	files = append(files, cpioFile{
		name: "assets/ephemerd-linux",
		mode: 0o100755,
		size: int64(len(ephemerdData)),
		data: ephemerdData,
	})
	fmt.Printf("  Bundling ephemerd-linux in initrd (%d bytes)\n", len(ephemerdData))

	// Hyper-V init script. On first boot, mkfs.ext4 on the single SCSI disk,
	// extract the bundled Alpine rootfs + ephemerd-linux onto it, and create a
	// 4 GiB swapfile. On subsequent boots, just mount + swapon. The entire OS
	// runs from disk (ext4 on VHDX), not tmpfs, so a 4 GiB VM can unpack
	// multi-GB OCI images without OOMing.
	initScript := `#!/bin/sh
# ephemerd initrd init script (Hyper-V, disk-backed root + swap)

mount -t proc     none /proc
mount -t sysfs    none /sys
mount -t devtmpfs none /dev

# Load kernel modules. hv_vmbus and hv_netvsc are built into the linux-virt
# kernel, but hv_storvsc (and sd_mod) are shipped as modules and must be
# insmod'd here for /dev/sda to appear. Also load filesystem + netfilter.
echo "ephemerd-init: loading kernel modules"
for mod in __MODULE_LOAD_ORDER__; do
    if [ -f /modules/${mod}.ko.gz ]; then
        gunzip -c /modules/${mod}.ko.gz > /tmp/${mod}.ko
        insmod /tmp/${mod}.ko || echo "ephemerd-init: insmod ${mod} failed (may already be loaded)"
        rm -f /tmp/${mod}.ko
    fi
done

# Give devtmpfs a moment to populate /dev after modules attach
sleep 1

# Parse kernel command line
CONTAINERD_PORT="10000"
ROOT_DISK=""
DIND="0"
for param in $(cat /proc/cmdline); do
    case "$param" in
        ephemerd.containerd_port=*) CONTAINERD_PORT="${param#*=}" ;;
        ephemerd.root_disk=*) ROOT_DISK="${param#*=}" ;;
        ephemerd.dind=1) DIND="1" ;;
    esac
done
echo "ephemerd-init: containerd_port=$CONTAINERD_PORT root_disk=$ROOT_DISK"

# Network: eth0 via hv_netvsc (built-in), DHCP from Default Switch
NET_IF=""
for iface in eth0 ens3 enp0s3; do
    if [ -d /sys/class/net/$iface ]; then
        NET_IF=$iface
        break
    fi
done
if [ -n "$NET_IF" ]; then
    ip link set lo up
    ip link set "$NET_IF" up
    udhcpc -i "$NET_IF" -n -t 8 -s /usr/share/udhcpc/default.script
    # Get our IP and gateway. The udhcpc default.script already set the route.
    MY_IP=$(ip -4 addr show "$NET_IF" 2>/dev/null | sed -n 's/.*inet \([0-9.]*\).*/\1/p')
    # Extract gateway from routing table (busybox ip route output: "default via X.X.X.X dev ethN")
    GW=""
    for word in $(ip route 2>/dev/null); do
        case "$prev" in
            via) GW="$word"; break ;;
        esac
        prev="$word"
    done
    echo "EPHEMERD_IP=$MY_IP"
    echo "ephemerd-init: gateway=$GW"

    # Clock sync: hv_utils kernel module provides Hyper-V TimeSync IC
    # which automatically adjusts the VM clock from the host. Without it,
    # the VM inherits the host's RTC (local time) but interprets it as UTC,
    # causing GitHub API token validation failures due to clock skew.
    echo "ephemerd-init: clock=$(date -u)"
else
    echo "ephemerd-init: WARNING: no network interface found"
fi

# Root filesystem lives on the attached SCSI disk. tmpfs root was rejected
# because a CI runner unpacking multi-GB OCI images OOMs a memory-capped VM.
# Everything under / (Alpine userspace, ephemerd-linux, containerd state, job
# scratch) is on the VHDX; a 4 GiB swapfile absorbs transient memory pressure.
if [ -z "$ROOT_DISK" ]; then
    echo "ephemerd-init: FATAL: ephemerd.root_disk not set on kernel cmdline"
    exec /bin/sh
fi

# hv_storvsc is built into linux-virt but SCSI enumeration can trail the end
# of module loading by tens of seconds — the Hyper-V host is sometimes slow
# to attach the VHDX, especially when other VMs are also starting. 15s was
# too tight and intermittently tripped a FATAL on otherwise-fine boots.
# 60s gives plenty of headroom without measurably slowing the happy path
# (we exit the loop the instant the block device appears). Busybox sleep
# only takes integer seconds.
i=0
while [ ! -b "$ROOT_DISK" ] && [ $i -lt 60 ]; do
    sleep 1
    i=$((i + 1))
done
if [ ! -b "$ROOT_DISK" ]; then
    echo "ephemerd-init: FATAL: root disk $ROOT_DISK did not appear after 60s"
    ls -la /dev/sd* 2>/dev/null || echo "  no /dev/sd* devices present"
    exec /bin/sh
fi
echo "ephemerd-init: root disk $ROOT_DISK ready after ${i}s"

NEED_POPULATE=0
if ! /sbin/blkid "$ROOT_DISK" >/dev/null 2>&1; then
    # Unformatted disk — first boot.
    echo "ephemerd-init: formatting $ROOT_DISK (first boot)"
    /sbin/mkfs.ext4 -q -L ephemerd-root -F "$ROOT_DISK" || {
        echo "ephemerd-init: FATAL: mkfs.ext4 failed"
        exec /bin/sh
    }
    NEED_POPULATE=1
fi

echo "ephemerd-init: mounting $ROOT_DISK at /newroot"
if ! mount -t ext4 -o noatime "$ROOT_DISK" /newroot; then
    echo "ephemerd-init: FATAL: mount $ROOT_DISK failed"
    exec /bin/sh
fi

# If the disk had a filesystem but the ephemerd rootfs was never populated
# (e.g. leftover from an older schema that only stored containerd data),
# treat it as first-boot and repopulate. The ephemerd-linux binary's presence
# is the marker for a valid rootfs.
if [ ! -x /newroot/usr/local/bin/ephemerd-linux ]; then
    NEED_POPULATE=1
fi

if [ "$NEED_POPULATE" = "1" ]; then
    if [ ! -f /assets/rootfs.tar.gz ] || [ ! -f /assets/ephemerd-linux ]; then
        echo "ephemerd-init: FATAL: bundled assets missing (/assets/rootfs.tar.gz, /assets/ephemerd-linux)"
        exec /bin/sh
    fi
    echo "ephemerd-init: populating rootfs on $ROOT_DISK"
    tar xzf /assets/rootfs.tar.gz -C /newroot
    mkdir -p /newroot/usr/local/bin

    # Create a 4 GiB swapfile so a 4 GiB VM can survive image-unpack peaks.
    # dd is a busybox applet and handles sparse allocation fine on ext4.
    echo "ephemerd-init: creating 4 GiB swapfile"
    dd if=/dev/zero of=/newroot/swapfile bs=1M count=4096 status=none || \
        echo "ephemerd-init: WARNING: swapfile dd failed"
    chmod 600 /newroot/swapfile
    /sbin/mkswap /newroot/swapfile >/dev/null 2>&1 || \
        busybox mkswap /newroot/swapfile >/dev/null 2>&1 || \
        echo "ephemerd-init: WARNING: mkswap failed"
else
    echo "ephemerd-init: existing ephemerd rootfs on $ROOT_DISK"
fi

# Always overwrite ephemerd-linux from the bundled assets on every boot.
# The persistent root.vhdx caches user data (image content, container
# state) but the binary itself must track whatever's baked into the
# initrd so a fresh ephemerd build actually takes effect in the VM.
if [ -f /assets/ephemerd-linux ]; then
    echo "ephemerd-init: refreshing ephemerd-linux from initrd"
    cp /assets/ephemerd-linux /newroot/usr/local/bin/ephemerd-linux
    chmod 755 /newroot/usr/local/bin/ephemerd-linux
fi

# Activate swap every boot (no-op if unavailable).
if [ -f /newroot/swapfile ]; then
    /sbin/swapon /newroot/swapfile 2>/dev/null || \
        busybox swapon /newroot/swapfile 2>/dev/null || \
        echo "ephemerd-init: WARNING: swapon failed"
fi

# Enable IP forwarding so containers can route through the VM to the internet.
# This is a kernel-level setting that persists across switch_root.
echo 1 > /proc/sys/net/ipv4/ip_forward

# Ensure the mount points the switch_root target expects.
mkdir -p /newroot/proc /newroot/sys /newroot/dev /newroot/tmp \
         /newroot/var/lib/ephemerd/containerd/root /newroot/etc

# DNS config — gateway is the DNS resolver on Default Switch
if [ -n "$GW" ]; then
    echo "nameserver $GW" > /newroot/etc/resolv.conf
fi

mount --move /proc /newroot/proc
mount --move /sys  /newroot/sys
mount --move /dev  /newroot/dev

mkdir -p /newroot/sys/fs/cgroup
mount -t cgroup2 none /newroot/sys/fs/cgroup || \
    echo "ephemerd-init: WARNING: cgroup2 mount failed"

export PATH=/usr/bin:/usr/sbin:/bin:/sbin
export HOME=/root

DIND_FLAG=""
if [ "$DIND" = "1" ]; then
    DIND_FLAG="--dind"
fi
echo "ephemerd-init: launching ephemerd-linux (dind=$DIND)"
exec switch_root /newroot /usr/local/bin/ephemerd-linux serve \
    --data-dir /var/lib/ephemerd \
    --containerd-tcp-port "$CONTAINERD_PORT" \
    --containerd-tcp-addr 0.0.0.0 \
    --containerd-only $DIND_FLAG
`
	// Substitute the resolved module load order
	modNames := make([]string, 0, len(loadOrder))
	for _, m := range loadOrder {
		base := m[strings.LastIndex(m, "/")+1:]
		base = strings.TrimSuffix(base, ".ko.gz")
		modNames = append(modNames, base)
	}
	initScript = strings.Replace(initScript, "__MODULE_LOAD_ORDER__", strings.Join(modNames, " "), 1)

	files = append(files, cpioFile{
		name: "init",
		mode: 0o100755,
		size: int64(len(initScript)),
		data: []byte(initScript),
	})

	// Write cpio archive (newc format) compressed with gzip
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating initrd: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dest)
		}
	}()

	gw := gzip.NewWriter(f)
	for _, cf := range files {
		if err := writeCPIOEntry(gw, cf.name, cf.mode, cf.data, cf.link); err != nil {
			if closeErr := gw.Close(); closeErr != nil {
				fmt.Printf("  warning: error closing gzip: %v\n", closeErr)
			}
			if closeErr := f.Close(); closeErr != nil {
				fmt.Printf("  warning: error closing file: %v\n", closeErr)
			}
			return fmt.Errorf("writing cpio entry %s: %w", cf.name, err)
		}
	}
	// Write TRAILER
	if err := writeCPIOEntry(gw, "TRAILER!!!", 0, nil, ""); err != nil {
		if closeErr := gw.Close(); closeErr != nil {
			fmt.Printf("  warning: error closing gzip: %v\n", closeErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			fmt.Printf("  warning: error closing file: %v\n", closeErr)
		}
		return fmt.Errorf("writing cpio trailer: %w", err)
	}
	if err := gw.Close(); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			fmt.Printf("  warning: error closing file: %v\n", closeErr)
		}
		return fmt.Errorf("closing gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing initrd file: %w", err)
	}

	fmt.Printf("  Created %s\n", dest)
	return nil
}

// writeCPIOEntry writes a single entry in newc (SVR4) cpio format.
func writeCPIOEntry(w io.Writer, name string, mode int64, data []byte, linkTarget string) error {
	var body []byte
	if linkTarget != "" {
		body = []byte(linkTarget)
	} else {
		body = data
	}
	nameBytes := append([]byte(name), 0) // null-terminated
	nameSize := len(nameBytes)
	fileSize := len(body)

	// newc header: 6 byte magic + 13 * 8 byte hex fields
	hdr := fmt.Sprintf("070701"+
		"%08X"+ // ino
		"%08X"+ // mode
		"%08X"+ // uid
		"%08X"+ // gid
		"%08X"+ // nlink
		"%08X"+ // mtime
		"%08X"+ // filesize
		"%08X"+ // devmajor
		"%08X"+ // devminor
		"%08X"+ // rdevmajor
		"%08X"+ // rdevminor
		"%08X"+ // namesize
		"%08X", // check
		0,        // ino
		mode,     // mode
		0,        // uid
		0,        // gid
		1,        // nlink
		0,        // mtime
		fileSize, // filesize
		0,        // devmajor
		0,        // devminor
		0,        // rdevmajor
		0,        // rdevminor
		nameSize, // namesize
		0,        // check
	)
	if _, err := io.WriteString(w, hdr); err != nil {
		return err
	}
	if _, err := w.Write(nameBytes); err != nil {
		return err
	}
	// Pad header+name to 4-byte boundary
	hdrLen := 110 + nameSize // 110 = magic(6) + 13*8
	if pad := (4 - hdrLen%4) % 4; pad > 0 {
		if _, err := w.Write(make([]byte, pad)); err != nil {
			return err
		}
	}
	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			return err
		}
		// Pad data to 4-byte boundary
		if pad := (4 - fileSize%4) % 4; pad > 0 {
			if _, err := w.Write(make([]byte, pad)); err != nil {
				return err
			}
		}
	}
	return nil
}

// extractAPKFile extracts a single file from an APK (tar.gz) by path.
func extractAPKFile(apkData []byte, filePath, dest string) error {
	br := bufio.NewReader(bytes.NewReader(apkData))
	gz, err := gzip.NewReader(br)
	if err != nil {
		return fmt.Errorf("reading apk gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)

	for {
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("reading apk tar: %w", err)
			}
			if hdr.Name == filePath {
				f, ferr := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
				if ferr != nil {
					return ferr
				}
				if _, ferr = io.Copy(f, tr); ferr != nil {
					_ = f.Close()
					return ferr
				}
				return f.Close()
			}
		}
		if err := gz.Reset(br); err != nil {
			break
		}
		gz.Multistream(false)
	}

	return fmt.Errorf("%s not found in APK", filePath)
}

// buildRootfsTarball creates a combined rootfs tarball from a base rootfs and APK packages.
func buildRootfsTarball(dest string, baseData []byte, pkgData [][]byte, packages []apkPkg) error {
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dest)
		}
	}()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	baseGz, err := gzip.NewReader(bytes.NewReader(baseData))
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("reading base rootfs: %w", err)
	}
	baseTr := tar.NewReader(baseGz)
	for {
		hdr, terr := baseTr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			_ = baseGz.Close()
			_ = tw.Close()
			_ = gw.Close()
			_ = f.Close()
			return fmt.Errorf("reading base rootfs tar: %w", terr)
		}
		if terr = tw.WriteHeader(hdr); terr != nil {
			_ = baseGz.Close()
			_ = tw.Close()
			_ = gw.Close()
			_ = f.Close()
			return fmt.Errorf("writing tar header: %w", terr)
		}
		if hdr.Size > 0 {
			if _, terr = io.Copy(tw, baseTr); terr != nil {
				_ = baseGz.Close()
				_ = tw.Close()
				_ = gw.Close()
				_ = f.Close()
				return fmt.Errorf("writing tar data: %w", terr)
			}
		}
	}
	_ = baseGz.Close()

	for i, data := range pkgData {
		if terr := appendAPKFiles(tw, data); terr != nil {
			_ = tw.Close()
			_ = gw.Close()
			_ = f.Close()
			return fmt.Errorf("extracting %s: %w", packages[i].name, terr)
		}
	}

	if err = tw.Close(); err != nil {
		_ = gw.Close()
		_ = f.Close()
		return fmt.Errorf("closing tar: %w", err)
	}
	if err = gw.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("closing gzip: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("closing output file: %w", err)
	}

	fmt.Printf("  Created %s\n", dest)
	return nil
}

// Golangcilint downloads golangci-lint to ./bin/.
func Golangcilint() error {
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}

	filename := fmt.Sprintf("golangci-lint-%s-%s-%s.%s", GolangCILintVersion, goos, goarch, ext)
	dest := filepath.Join(toolBinDir, "golangci-lint")
	if goos == "windows" {
		dest += ".exe"
	}

	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}

	if err := os.MkdirAll(toolBinDir, 0o755); err != nil {
		return err
	}

	url := fmt.Sprintf("https://github.com/golangci/golangci-lint/releases/download/v%s/%s", GolangCILintVersion, filename)
	fmt.Printf("  Downloading golangci-lint %s...\n", GolangCILintVersion)

	data, err := httpGetBytes(url)
	if err != nil {
		return fmt.Errorf("downloading golangci-lint: %w", err)
	}

	binName := "golangci-lint"
	if goos == "windows" {
		binName = "golangci-lint.exe"
	}
	prefix := fmt.Sprintf("golangci-lint-%s-%s-%s/", GolangCILintVersion, goos, goarch)

	if ext == "tar.gz" {
		gr, gerr := gzip.NewReader(bytes.NewReader(data))
		if gerr != nil {
			return fmt.Errorf("gzip golangci-lint: %w", gerr)
		}
		defer func() { _ = gr.Close() }()

		tr := tar.NewReader(gr)
		for {
			hdr, terr := tr.Next()
			if terr == io.EOF {
				return fmt.Errorf("golangci-lint binary not found in tarball")
			}
			if terr != nil {
				return fmt.Errorf("tar golangci-lint: %w", terr)
			}
			if hdr.Name == prefix+binName {
				f, ferr := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
				if ferr != nil {
					return ferr
				}
				if _, ferr = io.Copy(f, tr); ferr != nil {
					_ = f.Close()
					return ferr
				}
				return f.Close()
			}
		}
	}

	// zip extraction for Windows
	return extractZipBinary(data, prefix+binName, dest)
}

// extractZipBinary extracts a single file from a zip archive in memory.
func extractZipBinary(zipData []byte, entryName, dest string) error {
	r := bytes.NewReader(zipData)
	zr, err := zip.NewReader(r, int64(len(zipData)))
	if err != nil {
		return fmt.Errorf("opening zip: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != entryName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("opening zip entry: %w", err)
		}
		defer func() { _ = rc.Close() }()
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
	return fmt.Errorf("golangci-lint binary not found in zip")
}

// appendAPKFiles extracts data files from an APK package and appends them
// to the tar writer. APK files are concatenated gzip streams: signature,
// control (.PKGINFO etc.), and data (actual files). We process all streams
// but skip metadata entries (names starting with "." but not "./").
func appendAPKFiles(tw *tar.Writer, apkData []byte) error {
	br := bufio.NewReader(bytes.NewReader(apkData))
	gz, err := gzip.NewReader(br)
	if err != nil {
		return fmt.Errorf("reading apk gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)

	for {
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("reading apk tar: %w", err)
			}
			// Skip APK metadata entries (.PKGINFO, .SIGN.*, .pre-install, etc.)
			// Data entries start with "./" (filesystem paths like ./usr/lib/...)
			if strings.HasPrefix(hdr.Name, ".") && !strings.HasPrefix(hdr.Name, "./") {
				continue
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("writing tar header: %w", err)
			}
			if hdr.Size > 0 {
				if _, err := io.Copy(tw, tr); err != nil {
					return fmt.Errorf("writing tar data: %w", err)
				}
			}
		}

		// Advance to next gzip stream; EOF means no more streams
		if err := gz.Reset(br); err != nil {
			break
		}
		gz.Multistream(false)
	}

	return nil
}

// httpGetBytes downloads a URL and returns its body as a byte slice.
func httpGetBytes(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// linuxVirtMirrorURL returns the GitHub Releases mirror URL for the
// linux-virt APK pinned at LinuxVirtVersion. arch is "aarch64" or "x86_64".
// Note: GitHub Releases doesn't tolerate slashes in tag names (they 404
// the asset URL), so we use a dash-separated tag.
func linuxVirtMirrorURL(arch string) string {
	return fmt.Sprintf(
		"https://github.com/ephpm/ephemerd/releases/download/deps-linux-virt-%s/linux-virt-%s-%s.apk",
		LinuxVirtVersion, LinuxVirtVersion, arch,
	)
}

// linuxVirtSHA256 returns the pinned hash for the given arch.
func linuxVirtSHA256(arch string) string {
	switch arch {
	case "aarch64":
		return LinuxVirtSHA256AArch64
	case "x86_64":
		return LinuxVirtSHA256X86_64
	default:
		return ""
	}
}

// fetchLinuxVirtAPK downloads the linux-virt APK for arch and verifies its
// SHA256 against the pinned constant. arch is "aarch64" or "x86_64".
//
// Tries the mirror (a deps tag on this repo's GitHub Releases) first; falls
// back to upstream dl-cdn.alpinelinux.org if the mirror is unreachable
// (e.g. anonymous 404 on a private repo, deps tag deleted). The SHA256
// check still pins the bytes either way, so the upstream fallback is safe
// — it just stops working if Alpine prunes the revision, which is exactly
// what the mirror exists to insulate us from.
func fetchLinuxVirtAPK(arch string) ([]byte, error) {
	want := linuxVirtSHA256(arch)
	if want == "" {
		return nil, fmt.Errorf("linux-virt: unknown arch %q (want aarch64 or x86_64)", arch)
	}
	candidates := []string{
		linuxVirtMirrorURL(arch),
		fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/main/%s/linux-virt-%s.apk",
			alpineMajorMinor(AlpineVersion), arch, LinuxVirtVersion),
	}
	var lastErr error
	for _, url := range candidates {
		data, err := httpGetBytes(url)
		if err != nil {
			lastErr = err
			continue
		}
		got := fmt.Sprintf("%x", sha256.Sum256(data))
		if got != want {
			lastErr = fmt.Errorf("sha256 mismatch from %s: want %s, got %s",
				url, want, got)
			continue
		}
		return data, nil
	}
	return nil, fmt.Errorf("downloading linux-virt %s: %w", arch, lastErr)
}

func alpineMajorMinor(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return version
}

func downloadShim() error {
	return downloadShimForArch(cniArch(runtime.GOARCH))
}

func downloadShimForArch(arch string) error {
	dest := filepath.Join(shimEmbedDir, "containerd-shim-runc-v2")
	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}
	if err := os.MkdirAll(shimEmbedDir, 0o755); err != nil {
		return err
	}

	url := fmt.Sprintf("https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-%s.tar.gz",
		ContainerdVersion, ContainerdVersion, arch)
	fmt.Printf("  Downloading containerd-shim-runc-v2 from containerd %s...\n", ContainerdVersion)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download shim: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download shim: HTTP %d", resp.StatusCode)
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip shim: %w", err)
	}
	defer func() { _ = gr.Close() }()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("containerd-shim-runc-v2 not found in tarball")
		}
		if err != nil {
			return fmt.Errorf("tar shim: %w", err)
		}
		if strings.HasSuffix(hdr.Name, "bin/containerd-shim-runc-v2") {
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			return f.Close()
		}
	}
}

func downloadRunc() error {
	return downloadRuncForArch(cniArch(runtime.GOARCH))
}

func downloadRuncForArch(arch string) error {
	dest := filepath.Join(shimEmbedDir, "runc")
	url := fmt.Sprintf("https://github.com/opencontainers/runc/releases/download/v%s/runc.%s",
		RuncVersion, arch)
	if err := downloadFile(url, dest); err != nil {
		return err
	}
	return os.Chmod(dest, 0o755)
}

// downloadFile downloads url to dest, skipping if the file already exists.
func downloadFile(url, dest string) error {
	if fileExists(dest) {
		fmt.Printf("  %s already exists, skipping\n", dest)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	fmt.Printf("  Downloading %s...\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", dest, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", dest, resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	// Treat 0-byte files as missing — EnsurePlaceholders creates empty
	// files so go:embed compiles, but they must be replaced by real assets.
	return info.Size() > 0
}

// outOfDate reports whether dest needs to be rebuilt — true if it's missing,
// empty, or any of the listed input paths exists and is newer than dest.
// Inputs that don't exist are ignored (the build target hasn't produced them
// yet, so they can't make dest stale). The mtime comparison is what stops
// `mage build:windows` from happily embedding yesterday's ephemerd-linux into
// initrd because today's `download:Initrdx86` short-circuited on a file that
// already existed.
func outOfDate(dest string, inputs ...string) bool {
	destInfo, err := os.Stat(dest)
	if err != nil || destInfo.Size() == 0 {
		return true
	}
	for _, in := range inputs {
		ii, err := os.Stat(in)
		if err != nil {
			continue
		}
		if ii.ModTime().After(destInfo.ModTime()) {
			return true
		}
	}
	return false
}

func runnerPlatform(goos, goarch string) (os_, arch string) {
	switch goos {
	case "windows":
		os_ = "win"
	case "darwin":
		os_ = "osx"
	default:
		os_ = "linux"
	}
	if goarch == "arm64" {
		arch = "arm64"
	} else {
		arch = "x64"
	}
	return
}

func cniArch(goarch string) string {
	if goarch == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// alpineArch maps Go's GOARCH to Alpine's architecture naming.
func alpineArch(goarch string) string {
	if goarch == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

