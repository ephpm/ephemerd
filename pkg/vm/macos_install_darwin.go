//go:build darwin

package vm

/*
#cgo darwin LDFLAGS: -lcompression
#include <compression.h>
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"
)

// MacOSVMDiskFiles are the on-disk artifacts needed to boot macOS VMs.
// Pulled from a Tart OCI image (ghcr.io/cirruslabs/macos-*-base).
type MacOSVMDiskFiles struct {
	DataDir       string // e.g. /var/lib/ephemerd/vm/macos
	DiskImage     string // base.img — the bootable macOS VM disk
	AuxStorage    string // aux.bin  — NVRAM / auxiliary storage
	MachineID     string // machine-id.bin
	HardwareModel string // hardware-model.bin
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
	}
}

// MacOSInstallOptions configures how the base image is obtained.
type MacOSInstallOptions struct {
	// CustomDiskImage skips the Tart image pull entirely and uses this
	// pre-existing disk image. Set via vm.macos.disk_image in config.
	CustomDiskImage string

	// TartImage overrides the default Tart OCI image reference.
	// Default is auto-detected from the host macOS version
	// (e.g. ghcr.io/cirruslabs/macos-tahoe-base:latest).
	TartImage string

	// ImagesDir overrides the directory where macOS VM disk files are stored.
	// Defaults to <data_dir>/vm/macos.
	ImagesDir string
}

// EnsureMacOSVMDisk makes sure a bootable macOS disk image exists.
// If a custom disk image is configured, uses that. Otherwise pulls a
// pre-built Tart base image from ghcr.io. Idempotent.
func EnsureMacOSVMDisk(ctx context.Context, dataDir string, opts MacOSInstallOptions, log *slog.Logger) (*MacOSVMDiskFiles, error) {
	var files MacOSVMDiskFiles
	if opts.ImagesDir != "" {
		files = MacOSVMDiskFiles{
			DataDir:       opts.ImagesDir,
			DiskImage:     filepath.Join(opts.ImagesDir, "base.img"),
			AuxStorage:    filepath.Join(opts.ImagesDir, "aux.bin"),
			MachineID:     filepath.Join(opts.ImagesDir, "machine-id.bin"),
			HardwareModel: filepath.Join(opts.ImagesDir, "hardware-model.bin"),
		}
	} else {
		files = macOSVMFiles(dataDir)
	}
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
		log.Info("macOS base image already present — skipping pull", "path", files.DiskImage)
		// Re-inject LaunchDaemons if marker is missing (e.g. fresh pull
		// from a previous version, or scripts were updated).
		marker := filepath.Join(files.DataDir, ".provisioned-v7")
		if !fileExists(marker) {
			if err := injectLaunchDaemons(&files, log); err != nil {
				return nil, fmt.Errorf("injecting LaunchDaemons: %w", err)
			}
		}
		return &files, nil
	}

	// Determine which Tart image to pull
	imageRef := opts.TartImage
	if imageRef == "" {
		name, err := defaultTartImage()
		if err != nil {
			return nil, fmt.Errorf("detecting macOS version for Tart image: %w", err)
		}
		imageRef = name
	}

	log.Info("pulling macOS base image from Tart OCI registry",
		"image", imageRef,
		"note", "one-time download; subsequent boots are instant")

	if err := pullTartImage(ctx, imageRef, &files, log); err != nil {
		return nil, fmt.Errorf("pulling Tart image %s: %w", imageRef, err)
	}

	// Inject runner LaunchDaemon into the disk image so per-job VMs
	// auto-start the GitHub Actions runner from the virtio-fs share.
	if err := injectLaunchDaemons(&files, log); err != nil {
		return nil, fmt.Errorf("injecting LaunchDaemons: %w", err)
	}

	log.Info("macOS base image ready", "path", files.DiskImage)
	return &files, nil
}

// defaultTartImage returns the Tart base image reference matching
// the host's macOS major version.
func defaultTartImage() (string, error) {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return "", fmt.Errorf("sw_vers: %w", err)
	}
	version := strings.TrimSpace(string(out))
	major := strings.SplitN(version, ".", 2)[0]

	var codename string
	switch major {
	case "26":
		codename = "tahoe"
	case "15":
		codename = "sequoia"
	case "14":
		codename = "sonoma"
	case "13":
		codename = "ventura"
	default:
		// Future-proof: try tahoe for unknown versions
		codename = "tahoe"
	}

	return fmt.Sprintf("ghcr.io/cirruslabs/macos-%s-base:latest", codename), nil
}

// Tart OCI media types
const (
	tartConfigMediaType = "application/vnd.cirruslabs.tart.config.v1"
	tartDiskMediaType   = "application/vnd.cirruslabs.tart.disk.v2"
	tartNVRAMMediaType  = "application/vnd.cirruslabs.tart.nvram.v1"
)

// OCI manifest structures (subset needed for pull)
type ociManifest struct {
	Config ociDescriptor   `json:"config"`
	Layers []ociDescriptor `json:"layers"`
}

type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// tartConfig is the Tart VM config stored in the OCI config layer.
type tartConfig struct {
	OS            string          `json:"os"`
	Arch          string          `json:"arch"`
	HardwareModel string          `json:"hardwareModel"` // base64
	ECID          string          `json:"ecid"`          // base64 (machine identifier)
	Display       json.RawMessage `json:"display,omitempty"`
}

// pullTartImage pulls a Tart OCI image and extracts disk, NVRAM, and config.
func pullTartImage(ctx context.Context, imageRef string, files *MacOSVMDiskFiles, log *slog.Logger) error {
	// Parse image reference: ghcr.io/cirruslabs/macos-tahoe-base:latest
	registry, repo, tag, err := parseImageRef(imageRef)
	if err != nil {
		return err
	}

	// Get auth token (anonymous for public ghcr.io images)
	token, err := getRegistryToken(ctx, registry, repo)
	if err != nil {
		return fmt.Errorf("getting registry token: %w", err)
	}

	// Fetch manifest
	manifest, err := fetchManifest(ctx, registry, repo, tag, token)
	if err != nil {
		return fmt.Errorf("fetching manifest: %w", err)
	}

	// Process layers by media type
	var diskLayers []ociDescriptor
	var nvramLayer *ociDescriptor
	var configLayer *ociDescriptor

	for i := range manifest.Layers {
		switch manifest.Layers[i].MediaType {
		case tartDiskMediaType:
			diskLayers = append(diskLayers, manifest.Layers[i])
		case tartNVRAMMediaType:
			nvramLayer = &manifest.Layers[i]
		case tartConfigMediaType:
			configLayer = &manifest.Layers[i]
		}
	}

	if len(diskLayers) == 0 {
		return fmt.Errorf("no disk layers found in manifest")
	}
	if nvramLayer == nil {
		return fmt.Errorf("no NVRAM layer found in manifest")
	}

	// Pull config (from manifest config descriptor or config layer)
	var cfg tartConfig
	configDesc := configLayer
	if configDesc == nil {
		configDesc = &manifest.Config
	}
	cfgData, err := fetchBlob(ctx, registry, repo, configDesc.Digest, token)
	if err != nil {
		return fmt.Errorf("fetching config: %w", err)
	}
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	// Save hardware model and machine identifier
	if cfg.HardwareModel != "" {
		hwData, err := base64.StdEncoding.DecodeString(cfg.HardwareModel)
		if err != nil {
			return fmt.Errorf("decoding hardware model: %w", err)
		}
		if err := os.WriteFile(files.HardwareModel, hwData, 0o644); err != nil {
			return fmt.Errorf("writing hardware model: %w", err)
		}
	}
	if cfg.ECID != "" {
		ecidData, err := base64.StdEncoding.DecodeString(cfg.ECID)
		if err != nil {
			return fmt.Errorf("decoding ECID: %w", err)
		}
		if err := os.WriteFile(files.MachineID, ecidData, 0o644); err != nil {
			return fmt.Errorf("writing machine ID: %w", err)
		}
	}
	log.Info("saved VM platform config", "os", cfg.OS, "arch", cfg.Arch)

	// Pull NVRAM
	nvramData, err := fetchBlob(ctx, registry, repo, nvramLayer.Digest, token)
	if err != nil {
		return fmt.Errorf("fetching NVRAM: %w", err)
	}
	if err := os.WriteFile(files.AuxStorage, nvramData, 0o600); err != nil {
		return fmt.Errorf("writing NVRAM: %w", err)
	}
	log.Info("saved NVRAM", "size", len(nvramData))

	// Pull and decompress disk layers (LZ4-compressed, concatenated)
	diskFile, err := os.Create(files.DiskImage)
	if err != nil {
		return fmt.Errorf("creating disk image: %w", err)
	}
	defer func() {
		if err := diskFile.Close(); err != nil {
			log.Warn("closing disk image file", "error", err)
		}
	}()

	for i, layer := range diskLayers {
		log.Info("pulling disk layer",
			"layer", fmt.Sprintf("%d/%d", i+1, len(diskLayers)),
			"size_mb", layer.Size/(1024*1024),
			"digest", layer.Digest[:19])

		if err := pullAndDecompressDiskLayer(ctx, registry, repo, layer.Digest, token, diskFile, log); err != nil {
			if rmErr := os.Remove(files.DiskImage); rmErr != nil {
				log.Warn("removing partial disk image", "error", rmErr)
			}
			return fmt.Errorf("pulling disk layer %d: %w", i+1, err)
		}
	}

	log.Info("disk image assembled", "path", files.DiskImage)
	return nil
}

// pullAndDecompressDiskLayer fetches a single LZ4-compressed disk chunk
// and decompresses it directly to the output file using Apple's streaming
// Compression framework. Tart uses raw LZ4 (not frame format).
func pullAndDecompressDiskLayer(ctx context.Context, registry, repo, digest, token string, out *os.File, log *slog.Logger) error {
	const maxRetries = 3
	var lastErr error

	// Remember file position so we can rewind on retry
	startPos, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}

	for attempt := range maxRetries {
		if attempt > 0 {
			log.Info("retrying disk layer", "digest", digest[:19], "attempt", attempt+1)
			// Rewind to start of this layer
			if _, seekErr := out.Seek(startPos, io.SeekStart); seekErr != nil {
				log.Warn("seeking disk image for retry", "error", seekErr)
			}
			if truncErr := out.Truncate(startPos); truncErr != nil {
				log.Warn("truncating disk image for retry", "error", truncErr)
			}
		}
		lastErr = pullAndDecompressDiskLayerOnce(ctx, registry, repo, digest, token, out, log)
		if lastErr == nil {
			return nil
		}
		log.Warn("disk layer pull failed", "digest", digest[:19], "attempt", attempt+1, "error", lastErr)
	}
	return lastErr
}

func pullAndDecompressDiskLayerOnce(ctx context.Context, registry, repo, digest, token string, out *os.File, log *slog.Logger) error {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", registry, repo, digest)

	// Use a per-request context with a generous timeout for the entire
	// HTTP transaction (connect + TLS + response headers + body).
	// Individual read stalls are caught by stallDetectReader (30s).
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s fetching blob", resp.Status)
	}

	// Wrap the body with a stall-detecting reader — if no bytes arrive
	// for 30s, the read returns an error so we can retry the layer.
	stallReader := &stallDetectReader{r: resp.Body, timeout: 30 * time.Second}

	written, err := appleDecompressLZ4Stream(stallReader, out)
	if err != nil {
		return fmt.Errorf("decompressing: %w", err)
	}
	log.Debug("decompressed disk chunk", "decompressed", written)
	return nil
}

// stallDetectReader wraps an io.Reader and returns an error if a Read
// call blocks longer than timeout without returning data.
type stallDetectReader struct {
	r       io.Reader
	timeout time.Duration
}

func (s *stallDetectReader) Read(p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := s.r.Read(p)
		ch <- result{n, err}
	}()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-time.After(s.timeout):
		return 0, fmt.Errorf("read stalled for %s", s.timeout)
	}
}

// appleDecompressLZ4Stream decompresses an Apple Compression LZ4 stream
// from r to w using the streaming compression_stream API.
func appleDecompressLZ4Stream(r io.Reader, w io.Writer) (int64, error) {
	// Use C-allocated buffers to avoid CGo pointer restrictions.
	const srcBufSize = 256 * 1024
	const dstBufSize = 1024 * 1024
	srcBuf := C.malloc(C.size_t(srcBufSize))
	dstBuf := C.malloc(C.size_t(dstBufSize))
	defer C.free(srcBuf)
	defer C.free(dstBuf)

	var stream C.compression_stream
	status := C.compression_stream_init(&stream, C.COMPRESSION_STREAM_DECODE, C.COMPRESSION_LZ4)
	if status != C.COMPRESSION_STATUS_OK {
		return 0, fmt.Errorf("compression_stream_init failed: %d", status)
	}
	defer C.compression_stream_destroy(&stream)

	// Go-side read buffer (we copy into C buffer after reading)
	goSrcBuf := make([]byte, srcBufSize)
	var totalWritten int64

	stream.dst_ptr = (*C.uint8_t)(dstBuf)
	stream.dst_size = dstBufSize

	for {
		if stream.src_size == 0 {
			n, err := r.Read(goSrcBuf)
			if n > 0 {
				C.memcpy(srcBuf, unsafe.Pointer(&goSrcBuf[0]), C.size_t(n))
				stream.src_ptr = (*C.uint8_t)(srcBuf)
				stream.src_size = C.size_t(n)
			}
			if err == io.EOF {
				stream.dst_ptr = (*C.uint8_t)(dstBuf)
				stream.dst_size = dstBufSize
				_ = C.compression_stream_process(&stream, C.COMPRESSION_STREAM_FINALIZE)
				produced := dstBufSize - int(stream.dst_size)
				if produced > 0 {
					if _, err := w.Write(C.GoBytes(dstBuf, C.int(produced))); err != nil {
						return totalWritten, err
					}
					totalWritten += int64(produced)
				}
				return totalWritten, nil
			}
			if err != nil {
				return totalWritten, err
			}
		}

		stream.dst_ptr = (*C.uint8_t)(dstBuf)
		stream.dst_size = dstBufSize

		status = C.compression_stream_process(&stream, 0)
		produced := dstBufSize - int(stream.dst_size)
		if produced > 0 {
			if _, err := w.Write(C.GoBytes(dstBuf, C.int(produced))); err != nil {
				return totalWritten, err
			}
			totalWritten += int64(produced)
		}

		if status == C.COMPRESSION_STATUS_END {
			return totalWritten, nil
		}
		if status == C.COMPRESSION_STATUS_ERROR {
			return totalWritten, fmt.Errorf("compression_stream_process error at %d bytes", totalWritten)
		}
	}
}

// parseImageRef splits "ghcr.io/cirruslabs/macos-tahoe-base:latest"
// into registry, repository, and tag.
func parseImageRef(ref string) (registry, repo, tag string, err error) {
	// Split tag
	tag = "latest"
	if i := strings.LastIndex(ref, ":"); i > 0 {
		// Make sure this isn't part of the registry (e.g., localhost:5000)
		afterColon := ref[i+1:]
		if !strings.Contains(afterColon, "/") {
			tag = afterColon
			ref = ref[:i]
		}
	}

	// Split registry from repo
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid image reference: %s", ref)
	}
	registry = parts[0]
	repo = parts[1]
	return registry, repo, tag, nil
}

// getRegistryToken gets an anonymous bearer token for pulling from ghcr.io.
func getRegistryToken(ctx context.Context, registry, repo string) (string, error) {
	url := fmt.Sprintf("https://%s/token?service=%s&scope=repository:%s:pull", registry, registry, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token request failed: HTTP %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Token, nil
}

// fetchManifest retrieves the OCI manifest for the given image.
func fetchManifest(ctx context.Context, registry, repo, tag, token string) (*ociManifest, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var m ociManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// fetchBlob retrieves a blob from the registry into memory.
func fetchBlob(ctx context.Context, registry, repo, digest, token string) ([]byte, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", registry, repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}

// injectLaunchDaemons mounts the base image and writes the runner + scripts
// so per-job VMs have everything pre-installed. This runs once against the
// base image — clones inherit all files via APFS copy-on-write.
// We NEVER mount clones (hdiutil modifies APFS journal, breaking Vz boot).
func injectLaunchDaemons(files *MacOSVMDiskFiles, log *slog.Logger) error {
	dataVolume, detach, err := mountBaseImage(files.DiskImage, log)
	if err != nil {
		return fmt.Errorf("mounting base image: %w", err)
	}
	// detach is called explicitly before consolidation, not deferred.
	detachOnError := true
	defer func() {
		if detachOnError {
			detach()
		}
	}()
	log.Info("mounted macOS data volume for injection", "path", dataVolume)

	// Inject the GitHub Actions runner into /Users/admin/actions-runner/
	// so every per-job clone has it pre-installed (no SSH tar copy needed).
	runnerDir := ""
	// files.DataDir is <dataDir>/vm/macos — runners are at <dataDir>/runners/
	dataDir := filepath.Dir(filepath.Dir(files.DataDir))
	matches, _ := filepath.Glob(filepath.Join(dataDir, "runners", "*"))
	for _, d := range matches {
		if _, err := os.Stat(filepath.Join(d, "run.sh")); err == nil {
			runnerDir = d
			break
		}
	}
	// Runner injection happens via SSH after a one-time provisioning boot
	// (see below). Host-side writes to the APFS Data volume are not visible
	// inside the VM due to APFS journal/transaction isolation.
	_ = runnerDir // used in provisionRunnerViaSSH

	launchDir := filepath.Join(dataVolume, "Library", "LaunchDaemons")
	if err := os.MkdirAll(launchDir, 0o755); err != nil {
		return fmt.Errorf("creating LaunchDaemons dir: %w", err)
	}

	scriptDir := filepath.Join(dataVolume, "Library", "ephemerd")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		return fmt.Errorf("creating ephemerd script dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(scriptDir, "start-runner.sh"), []byte(macOSRunnerScript), 0o755); err != nil {
		return fmt.Errorf("writing runner script: %w", err)
	}

	if err := os.WriteFile(filepath.Join(launchDir, "com.ephemerd.runner.plist"), []byte(macOSRunnerPlist), 0o644); err != nil {
		return fmt.Errorf("writing runner plist: %w", err)
	}

	log.Info("wrote LaunchDaemon for runner startup")

	// Detach before consolidation — must unmount before rewriting the file.
	detach()
	detachOnError = false

	// Rewrite the image in-place to consolidate APFS extents.
	// This ensures cp -c clones see all the injected files.
	tmpPath := files.DiskImage + ".consolidated"
	if err := exec.Command("cp", files.DiskImage, tmpPath).Run(); err != nil {
		log.Warn("failed to consolidate base image, clones may not have injected files", "error", err)
	} else {
		if err := os.Rename(tmpPath, files.DiskImage); err != nil {
			log.Warn("failed to replace base image with consolidated copy", "error", err)
			_ = os.Remove(tmpPath)
		} else {
			log.Info("base image consolidated for APFS clone compatibility")
		}
	}

	marker := filepath.Join(files.DataDir, ".provisioned-v7")
	_ = os.WriteFile(marker, []byte("1"), 0o644)
	return nil
}

// mountBaseImage attaches the raw disk image via hdiutil, finds the APFS
// Data volume, mounts it, and returns the mount path + a detach cleanup func.
func mountBaseImage(diskPath string, log *slog.Logger) (string, func(), error) {
	noop := func() {}

	out, err := exec.Command("hdiutil", "attach",
		"-imagekey", "diskimage-class=CRawDiskImage",
		"-nomount", diskPath).CombinedOutput()
	if err != nil {
		return "", noop, fmt.Errorf("hdiutil attach: %s: %w", out, err)
	}

	// Find the physical disk device
	var physDisk string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 && strings.HasPrefix(fields[0], "/dev/disk") {
			if strings.Contains(line, "GUID_partition_scheme") {
				physDisk = fields[0]
				break
			}
		}
	}
	if physDisk == "" {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 1 && strings.HasPrefix(fields[0], "/dev/disk") {
				physDisk = fields[0]
				break
			}
		}
	}
	if physDisk == "" {
		return "", noop, fmt.Errorf("could not find disk device in hdiutil output: %s", out)
	}

	detach := func() {
		if err := exec.Command("hdiutil", "detach", physDisk).Run(); err != nil {
			log.Warn("failed to detach disk image", "disk", physDisk, "error", err)
		}
	}

	// Find the Data volume
	allOut, _ := exec.Command("diskutil", "list").CombinedOutput()
	var dataDevice string
	for _, line := range strings.Split(string(allOut), "\n") {
		if strings.Contains(line, "APFS Volume") && strings.Contains(line, "Data") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				dataDevice = "/dev/" + fields[len(fields)-1]
			}
			break
		}
	}

	if dataDevice == "" {
		detach()
		return "", noop, fmt.Errorf("could not find Data volume")
	}

	mountOut, err := exec.Command("diskutil", "mount", dataDevice).CombinedOutput()
	if err != nil {
		detach()
		return "", noop, fmt.Errorf("mounting Data volume %s: %s: %w", dataDevice, mountOut, err)
	}

	mountPoint := "/Volumes/Data"
	if strings.Contains(string(mountOut), "mounted at ") {
		parts := strings.SplitAfter(string(mountOut), "mounted at ")
		if len(parts) >= 2 {
			mountPoint = strings.TrimSpace(parts[1])
		}
	}

	return mountPoint, detach, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Runner startup script — runs on every per-job VM boot via LaunchDaemon.
// Mounts virtio-fs shares, installs ephemeral SSH key, starts GHA runner.
// Tart base images have user "admin" with password "admin" and sudo.
const macOSRunnerScript = `#!/bin/bash
# No set -e — we want to log failures, not silently exit.
exec &>/tmp/ephemerd-runner.log
echo "=== ephemerd runner script starting at $(date) ==="

SHARE="/Volumes/ephemerd"
RUNNER_MOUNT="/Volumes/runner"

# Harden FIRST (before mounting shares, no dependencies on virtio-fs).
# Randomize the default admin/admin password.
echo "hardening: randomizing admin password"
RAND_PASS=$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 32)
dscl . -passwd /Users/admin admin "$RAND_PASS" 2>&1 || echo "dscl passwd failed: $?"

# Disable SSH password auth via per-user sshd config (system sshd_config
# is on the signed system volume and cannot be modified).
echo "hardening: configuring SSH key-only auth"
mkdir -p /etc/ssh/sshd_config.d 2>/dev/null || true
cat > /etc/ssh/sshd_config.d/ephemerd.conf 2>/dev/null <<'SSHEOF'
PasswordAuthentication no
ChallengeResponseAuthentication no
SSHEOF

# Mount virtio-fs shares — macOS does NOT auto-mount them.
echo "mounting virtio-fs shares"
mkdir -p "$SHARE" "$RUNNER_MOUNT"

SHARE_MOUNTED=false
for i in $(seq 1 60); do
    if mount_virtiofs ephemerd "$SHARE" 2>&1; then
        echo "ephemerd share mounted on attempt $i"
        SHARE_MOUNTED=true
        break
    fi
    sleep 1
done
if [ "$SHARE_MOUNTED" = false ]; then
    echo "ERROR: failed to mount ephemerd share after 60 attempts"
    echo "mount_virtiofs output: $(mount_virtiofs ephemerd "$SHARE" 2>&1)"
    echo "available virtio devices: $(ls /dev/*virtio* 2>&1)"
    echo "all mounts: $(mount)"
    exit 1
fi

RUNNER_MOUNTED=false
for i in $(seq 1 60); do
    if mount_virtiofs runner "$RUNNER_MOUNT" 2>&1; then
        echo "runner share mounted on attempt $i"
        RUNNER_MOUNTED=true
        break
    fi
    sleep 1
done
if [ "$RUNNER_MOUNTED" = false ]; then
    echo "ERROR: failed to mount runner share after 60 attempts"
fi

JIT_CONFIG="$SHARE/.jit_config"
READY_FILE="$SHARE/.ready"
SSH_PUBKEY="$SHARE/.ssh_pubkey"

# Install the ephemeral SSH public key (rotates every daemon restart)
if [ -f "$SSH_PUBKEY" ]; then
    echo "installing ephemeral SSH public key"
    mkdir -p /Users/admin/.ssh
    cp "$SSH_PUBKEY" /Users/admin/.ssh/authorized_keys
    chmod 600 /Users/admin/.ssh/authorized_keys
    chown -R admin:staff /Users/admin/.ssh
else
    echo "WARNING: no SSH pubkey found at $SSH_PUBKEY"
    ls -la "$SHARE/" 2>&1
fi

# Wait for runner
if [ "$RUNNER_MOUNTED" = true ] && [ -f "$RUNNER_MOUNT/run.sh" ]; then
    echo "runner found at $RUNNER_MOUNT/run.sh"
else
    echo "waiting for runner at $RUNNER_MOUNT/run.sh"
    for i in $(seq 1 60); do
        [ -f "$RUNNER_MOUNT/run.sh" ] && break
        sleep 1
    done
fi

if [ ! -f "$RUNNER_MOUNT/run.sh" ]; then
    echo "ERROR: runner not found at $RUNNER_MOUNT/run.sh"
    ls -la "$RUNNER_MOUNT/" 2>&1
    exit 1
fi

# Copy runner to local disk (runner share is read-only)
RUNNER_DIR="/Users/admin/actions-runner"
if [ ! -f "$RUNNER_DIR/run.sh" ]; then
    echo "copying runner to $RUNNER_DIR"
    mkdir -p "$RUNNER_DIR"
    cp -R "$RUNNER_MOUNT/"* "$RUNNER_DIR/"
    chown -R admin:staff "$RUNNER_DIR"
    chmod -R u+w "$RUNNER_DIR"
    echo "runner copied ($(du -sh "$RUNNER_DIR" | cut -f1))"
fi

# Wait for JIT config
echo "waiting for JIT config at $JIT_CONFIG"
for i in $(seq 1 120); do
    [ -f "$JIT_CONFIG" ] && break
    sleep 1
done

if [ ! -f "$JIT_CONFIG" ]; then
    echo "ERROR: JIT config not found at $JIT_CONFIG after 120s"
    ls -la "$SHARE/" 2>&1
    exit 1
fi

cd "$RUNNER_DIR"
ENCODED_JIT=$(cat "$JIT_CONFIG")

# Signal readiness
echo "signaling readiness via $READY_FILE"
touch "$READY_FILE"

# Run the GitHub Actions runner
echo "starting runner at $(date)"
sudo -u admin ./run.sh --jitconfig "$ENCODED_JIT" 2>&1 || echo "runner exited: $?"
echo "=== ephemerd runner script finished at $(date) ==="
`

const macOSRunnerPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.ephemerd.runner</string>
    <key>ProgramArguments</key>
    <array>
        <string>/Library/ephemerd/start-runner.sh</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/ephemerd-runner.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/ephemerd-runner.log</string>
</dict>
</plist>
`
