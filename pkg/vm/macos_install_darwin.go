//go:build darwin

package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// MacOSVMDiskFiles are the on-disk artifacts produced by installing macOS
// into a Vz-bootable disk. Per-job VMs APFS-clone DiskImage and reuse the
// rest directly.
type MacOSVMDiskFiles struct {
	DataDir       string // e.g. /var/lib/ephemerd/vm/macos
	DiskImage     string // base.img — the bootable macOS VM disk
	AuxStorage    string // aux.bin  — boot-time auxiliary storage
	MachineID     string // machine-id.bin
	HardwareModel string // hardware-model.bin
	RestoreIPSW   string // restore.ipsw — deleted after a successful install
}

// macOSVMFiles returns the canonical paths under dataDir.
func macOSVMFiles(dataDir string) MacOSVMDiskFiles {
	dir := filepath.Join(dataDir, "vm", "macos")
	return MacOSVMDiskFiles{
		DataDir:       dir,
		DiskImage:     filepath.Join(dir, "base.img"),
		AuxStorage:    filepath.Join(dir, "aux.bin"),
		MachineID:     filepath.Join(dir, "machine-id.bin"),
		HardwareModel: filepath.Join(dir, "hardware-model.bin"),
		RestoreIPSW:   filepath.Join(dir, "restore.ipsw"),
	}
}

// MacOSInstallOptions tunes the one-time install.
type MacOSInstallOptions struct {
	// CustomDiskImage overrides the default macOS VM disk path. If empty,
	// EnsureMacOSBaseImage uses <dataDir>/vm/macos/base.img. Set this to
	// a pre-installed disk image (e.g. one produced on another host, or
	// restored from a backup) and the auto-install is still skipped if
	// the file already exists there.
	CustomDiskImage string
	// DiskSizeGB is the size of the provisioned macOS disk. 40 GB is enough
	// for a stock install plus typical CI workloads; bump if your jobs need
	// more room for SDKs/toolchains that get layered on at job time.
	DiskSizeGB uint64
	// CPUs allocated to the one-time installer VM. Subsequent per-job VMs
	// are sized separately via MacOSVMConfig.
	CPUs uint
	// MemoryMB allocated to the installer VM (minimum enforced by macOS).
	MemoryMB uint64
}

// EnsureMacOSVMDisk makes sure <dataDir>/vm/macos/base.img exists and is
// a bootable macOS disk. If it's missing, downloads the latest Apple-signed
// restore image / IPSW (~14 GB) and runs the Vz macOS installer (~30 min).
// Idempotent: returns nil quickly when the disk image is already in place.
//
// The caller should surface this long delay to operators (docs + log
// breadcrumbs) — on a fresh host this blocks ephemerd startup until the
// install finishes, because per-job VMs need the disk image before they
// can boot. Not to be confused with the OCI base images that jobs overlay
// onto the running VM via virtio-fs — this is the Vz-bootable disk.
func EnsureMacOSVMDisk(ctx context.Context, dataDir string, opts MacOSInstallOptions, log *slog.Logger) (*MacOSVMDiskFiles, error) {
	files := macOSVMFiles(dataDir)
	if opts.CustomDiskImage != "" {
		files.DiskImage = opts.CustomDiskImage
	}
	if err := os.MkdirAll(files.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", files.DataDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(files.DiskImage), 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", filepath.Dir(files.DiskImage), err)
	}

	if fileExists(files.DiskImage) {
		log.Info("macOS base image already present — skipping install", "path", files.DiskImage)
		return &files, nil
	}

	if opts.DiskSizeGB == 0 {
		opts.DiskSizeGB = 40
	}
	if opts.CPUs == 0 {
		opts.CPUs = 4
	}
	if opts.MemoryMB == 0 {
		opts.MemoryMB = 8192
	}

	log.Info("no macOS base image found — fetching IPSW and installing",
		"data_dir", files.DataDir, "disk_gb", opts.DiskSizeGB,
		"note", "this is a one-time, ~30 min operation; subsequent boots are fast")

	if err := downloadIPSW(ctx, files.RestoreIPSW, log); err != nil {
		return nil, fmt.Errorf("downloading IPSW: %w", err)
	}

	image, err := vz.LoadMacOSRestoreImageFromPath(files.RestoreIPSW)
	if err != nil {
		return nil, fmt.Errorf("loading IPSW %s: %w", files.RestoreIPSW, err)
	}
	reqs := image.MostFeaturefulSupportedConfiguration()
	if reqs == nil {
		return nil, fmt.Errorf("IPSW %s: no supported configuration for this host", files.RestoreIPSW)
	}
	log.Info("IPSW loaded",
		"os_version", image.OperatingSystemVersion().String(),
		"build", image.BuildVersion(),
		"min_cpus", reqs.MinimumSupportedCPUCount(),
		"min_memory_mb", reqs.MinimumSupportedMemorySize()/(1024*1024))

	if uint64(opts.CPUs) < reqs.MinimumSupportedCPUCount() {
		opts.CPUs = uint(reqs.MinimumSupportedCPUCount())
	}
	if opts.MemoryMB*1024*1024 < reqs.MinimumSupportedMemorySize() {
		opts.MemoryMB = reqs.MinimumSupportedMemorySize() / (1024 * 1024)
	}

	hw := reqs.HardwareModel()
	if err := os.WriteFile(files.HardwareModel, hw.DataRepresentation(), 0o644); err != nil {
		return nil, fmt.Errorf("saving hardware model: %w", err)
	}

	machineID, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return nil, fmt.Errorf("creating machine identifier: %w", err)
	}
	if err := os.WriteFile(files.MachineID, machineID.DataRepresentation(), 0o644); err != nil {
		return nil, fmt.Errorf("saving machine identifier: %w", err)
	}

	aux, err := vz.NewMacAuxiliaryStorage(files.AuxStorage, vz.WithCreatingMacAuxiliaryStorage(hw))
	if err != nil {
		return nil, fmt.Errorf("creating aux storage: %w", err)
	}

	if err := createSparseDisk(files.DiskImage, opts.DiskSizeGB); err != nil {
		return nil, fmt.Errorf("creating base disk: %w", err)
	}

	vm, err := buildInstallerVM(files.DiskImage, aux, hw, machineID, opts)
	if err != nil {
		return nil, fmt.Errorf("building installer VM: %w", err)
	}

	installer, err := vz.NewMacOSInstaller(vm, files.RestoreIPSW)
	if err != nil {
		return nil, fmt.Errorf("creating installer: %w", err)
	}

	log.Info("starting macOS install — this may take 20–45 minutes")
	installCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pct := installer.FractionCompleted() * 100
				log.Info("macOS install progress", "percent", fmt.Sprintf("%.1f", pct))
			case <-installer.Done():
				return
			case <-installCtx.Done():
				return
			}
		}
	}()

	if err := installer.Install(installCtx); err != nil {
		return nil, fmt.Errorf("macOS install: %w", err)
	}
	<-progressDone
	log.Info("macOS install complete", "disk", files.DiskImage)

	if err := os.Remove(files.RestoreIPSW); err != nil {
		log.Warn("could not remove IPSW after install", "path", files.RestoreIPSW, "error", err)
	} else {
		log.Info("removed IPSW after install", "path", files.RestoreIPSW)
	}

	return &files, nil
}

// downloadIPSW fetches the latest Apple-signed macOS restore image into
// destPath and logs download progress every 30s so operators watching the
// daemon log can see what's happening.
func downloadIPSW(ctx context.Context, destPath string, log *slog.Logger) error {
	if fileExists(destPath) {
		log.Info("IPSW already downloaded — reusing", "path", destPath)
		return nil
	}
	log.Info("downloading macOS IPSW from Apple — this is ~14 GB", "dest", destPath)

	// Download to a .part file first so a crashed/interrupted download
	// doesn't leave a file LoadMacOSRestoreImageFromPath can't parse.
	// The Vz library resumes from the .part file's current size via HTTP
	// Range headers, so retries don't re-download from scratch.
	partPath := destPath + ".part"

	const maxRetries = 5
	var lastErr error
	for attempt := range maxRetries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			backoff := time.Duration(attempt) * 30 * time.Second
			log.Info("retrying IPSW download (resumes from last position)",
				"attempt", attempt+1, "backoff", backoff.String())
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		lastErr = downloadIPSWOnce(ctx, partPath, log)
		if lastErr == nil {
			if err := os.Rename(partPath, destPath); err != nil {
				return fmt.Errorf("renaming IPSW: %w", err)
			}
			log.Info("IPSW download complete", "path", destPath)
			return nil
		}
		log.Warn("IPSW download failed", "attempt", attempt+1, "error", lastErr)
	}
	return fmt.Errorf("IPSW download failed after %d attempts: %w", maxRetries, lastErr)
}

func downloadIPSWOnce(ctx context.Context, partPath string, log *slog.Logger) error {
	url, err := vz.GetLatestSupportedMacOSRestoreImageURL()
	if err != nil {
		return fmt.Errorf("getting IPSW URL: %w", err)
	}

	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return err
	}
	offset := stat.Size()
	if offset > 0 {
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
		log.Info("resuming IPSW download", "offset", offset)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected HTTP status: %s", resp.Status)
	}

	totalSize := int64(0)
	if resp.ContentLength > 0 {
		totalSize = offset + resp.ContentLength
	}
	written := offset

	const stallTimeout = 30 * time.Second
	buf := make([]byte, 256*1024)
	lastProgress := time.Now()
	lastLog := time.Now()

	for {
		if time.Since(lastProgress) > stallTimeout {
			return fmt.Errorf("download stalled — no data received for %s", stallTimeout)
		}

		deadline := time.Now().Add(stallTimeout)
		if tc, ok := resp.Body.(interface{ SetReadDeadline(time.Time) error }); ok {
			tc.SetReadDeadline(deadline)
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			written += int64(n)
			lastProgress = time.Now()
		}

		if time.Since(lastLog) >= 10*time.Second {
			pct := float64(0)
			if totalSize > 0 {
				pct = float64(written) / float64(totalSize) * 100
			}
			log.Info("IPSW download progress",
				"percent", fmt.Sprintf("%.1f", pct),
				"bytes", written)
			lastLog = time.Now()
		}

		if readErr != nil {
			if readErr == io.EOF {
				log.Info("IPSW download progress", "percent", "100.0", "bytes", written)
				return nil
			}
			return fmt.Errorf("download failed at %d bytes: %w", written, readErr)
		}
	}
}

// buildInstallerVM constructs the VM that runs the macOS installer. It's a
// standard Mac VM config pointing at the (empty) base disk and the aux
// storage we just initialized; the installer mutates both in place.
func buildInstallerVM(diskPath string, aux *vz.MacAuxiliaryStorage, hw *vz.MacHardwareModel, machineID *vz.MacMachineIdentifier, opts MacOSInstallOptions) (*vz.VirtualMachine, error) {
	bootLoader, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}
	vmCfg, err := vz.NewVirtualMachineConfiguration(bootLoader, opts.CPUs, opts.MemoryMB*1024*1024)
	if err != nil {
		return nil, fmt.Errorf("vm config: %w", err)
	}

	platform, err := vz.NewMacPlatformConfiguration(
		vz.WithMacHardwareModel(hw),
		vz.WithMacMachineIdentifier(machineID),
		vz.WithMacAuxiliaryStorage(aux),
	)
	if err != nil {
		return nil, fmt.Errorf("platform: %w", err)
	}
	vmCfg.SetPlatformVirtualMachineConfiguration(platform)

	// Headless graphics (Vz still requires a graphics device configured)
	graphics, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("graphics: %w", err)
	}
	display, err := vz.NewMacGraphicsDisplayConfiguration(1920, 1200, 80)
	if err != nil {
		return nil, fmt.Errorf("display: %w", err)
	}
	graphics.SetDisplays(display)
	vmCfg.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{graphics})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("entropy: %w", err)
	}
	vmCfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("nat: %w", err)
	}
	netCfg, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, fmt.Errorf("net config: %w", err)
	}
	vmCfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netCfg})

	diskAttach, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		diskPath, false, vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFsync,
	)
	if err != nil {
		return nil, fmt.Errorf("disk attach: %w", err)
	}
	blockDev, err := vz.NewVirtioBlockDeviceConfiguration(diskAttach)
	if err != nil {
		return nil, fmt.Errorf("block device: %w", err)
	}
	vmCfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blockDev})

	ok, err := vmCfg.Validate()
	if err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	if !ok {
		return nil, errors.New("installer VM config invalid")
	}
	return vz.NewVirtualMachine(vmCfg)
}

// createSparseDisk makes an empty sparse file of the requested size. macOS
// uses APFS on top; the bytes don't need to be zeroed up front.
func createSparseDisk(path string, sizeGB uint64) error {
	if fileExists(path) {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(int64(sizeGB) * 1024 * 1024 * 1024)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
