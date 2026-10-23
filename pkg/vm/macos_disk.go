package vm

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"
)

// Per-job macOS disk growth.
//
// Every macOS job boots from an APFS clone of base.img, so the guest gets
// whatever the base image was built with — 50 GB for the Tart image this
// fleet uses, ~41 GiB usable once macOS is on it. That is too small for
// ePHPm's nightly PHP-linked test build (each test binary embeds libphp +
// ICU), which filled the guest every night with "No space left on device".
//
// Growing it takes two steps, one on each side of the boot:
//
//  1. Host, before boot: extend the clone file (os.Truncate). On APFS this is
//     sparse — it costs nothing until the guest writes — and the blocks the
//     clone shares with base.img stay shared.
//  2. Guest, over SSH: `diskutil repairDisk` moves the GPT's backup header to
//     the new end of the disk, then `diskutil apfs resizeContainer <store> 0`
//     grows the boot container into the space. This only works when nothing
//     sits after the container; the fleet's base image (verified 2026-10-22)
//     is ISC (524 MB), the APFS container (44.1 GB), then free space — no
//     Recovery partition in the way.
//
// Step 2 is best-effort. A failure is logged and the job runs on the disk it
// would have had anyway; it never fails the job.

// gib is one GiB. disk_size_gb is GiB, matching the Linux VM's DiskSizeGB.
const gib = 1 << 30

// macOSDiskGrowTimeout bounds each guest-side resize command. It stays well
// inside the 5 minute provisioning deadline: a wedged diskutil must cost the
// job its extra space, never the job itself.
const macOSDiskGrowTimeout = 90 * time.Second

// ResolveMacOSDiskSize validates the configured macOS job disk size against
// the base image and returns the size to use, in GiB. 0 means "leave the
// clone exactly as big as the base image" — the default, and what is
// returned for any value that would not grow the disk. A shrink is never
// attempted: truncating below the base image would cut off the guest's
// filesystem.
func ResolveMacOSDiskSize(baseImage string, wantGB uint64, log *slog.Logger) uint64 {
	if wantGB == 0 {
		return 0
	}
	fi, err := os.Stat(baseImage)
	if err != nil {
		log.Warn("vm.macos.disk_size_gb ignored: cannot read the base image size",
			"disk_size_gb", wantGB, "base_image", baseImage, "error", err)
		return 0
	}
	if _, grow := macOSDiskGrowBytes(fi.Size(), wantGB); !grow {
		log.Warn("vm.macos.disk_size_gb ignored: not larger than the base image, and the disk is never shrunk",
			"disk_size_gb", wantGB, "base_image_gib", fmt.Sprintf("%.1f", float64(fi.Size())/gib))
		return 0
	}
	return wantGB
}

// macOSDiskGrowBytes returns the byte size a clone of curSize bytes should be
// truncated to for a wantGB GiB disk, and whether that is a growth at all.
func macOSDiskGrowBytes(curSize int64, wantGB uint64) (int64, bool) {
	if wantGB == 0 {
		return curSize, false
	}
	want := int64(wantGB) * gib
	if want <= curSize {
		return curSize, false
	}
	return want, true
}

// diskIdentRe accepts only plain diskutil partition identifiers. The value is
// pasted into a guest shell command, so anything else is refused.
var diskIdentRe = regexp.MustCompile(`^disk[0-9]+s[0-9]+$`)

// parseAPFSPhysicalStore extracts the boot container's physical store (the
// GPT partition, e.g. "disk0s2") and its whole disk ("disk0") from
// `diskutil info` output for a volume in that container.
//
// It asks rather than assuming disk0s2: the partition layout belongs to the
// base image, which is not ours, and resizing the wrong partition is not a
// mistake worth risking.
func parseAPFSPhysicalStore(info string) (store, whole string, err error) {
	for _, line := range strings.Split(info, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || strings.TrimSpace(k) != "APFS Physical Store" {
			continue
		}
		store = strings.TrimSpace(v)
		if !diskIdentRe.MatchString(store) {
			return "", "", fmt.Errorf("unexpected APFS physical store %q", store)
		}
		return store, store[:strings.LastIndex(store, "s")], nil
	}
	return "", "", fmt.Errorf("no \"APFS Physical Store\" line in diskutil info output")
}

// macOSDiskInfoCmd reads the boot container's details. The Data volume is a
// plain APFS volume in the same container as the sealed system snapshot at /,
// so it is asked first; / is the fallback.
const macOSDiskInfoCmd = `diskutil info /System/Volumes/Data 2>/dev/null || diskutil info /`

// macOSDiskGrowScript grows the boot container into the space step 1 added.
// It takes the whole disk and the physical store, both validated by
// parseAPFSPhysicalStore, and prints one status line with the root
// filesystem's size before and after.
//
// repairDisk asks before rewriting the partition map, hence `yes |`; its exit
// status is not trusted on its own, because resizeContainer reports plainly
// whether the space is now usable.
func macOSDiskGrowScript(whole, store string) string {
	return fmt.Sprintf(`before=$(df -g / | awk 'NR==2{print $2}')
yes | sudo -n diskutil repairDisk %[1]s >/tmp/ephemerd-repairdisk.log 2>&1
if sudo -n diskutil apfs resizeContainer %[2]s 0 >/tmp/ephemerd-resize.log 2>&1; then r=ok; else r="failed: $(tail -n 3 /tmp/ephemerd-resize.log | tr '\n' ' ')"; fi
after=$(df -g / | awk 'NR==2{print $2}')
echo "before=${before}G after=${after}G resize=${r}"
`, whole, store)
}

// macOSBrewUntapScript removes the buildkite/buildkite Homebrew tap that the
// Tart base image ships. Nothing on the fleet uses it, and every brew command
// a job runs prints a warning that the tap is not trusted. It runs as the
// guest's admin user (the brew owner on Tart images), never as root, and
// prints one status word. --force untaps even though the tap's formula is
// installed; the installed binary is left alone.
const macOSBrewUntapScript = `B=/opt/homebrew/bin/brew; [ -x "$B" ] || B=/usr/local/bin/brew; [ -x "$B" ] || { echo no-brew; exit 0; }
export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 HOMEBREW_NO_ENV_HINTS=1
if "$B" tap 2>/dev/null | grep -qx buildkite/buildkite; then "$B" untap --force buildkite/buildkite >/dev/null 2>&1 && echo untapped || echo untap-failed; else echo absent; fi
`

// macOSBrewUntapTimeout bounds the untap. `brew tap` is a local listing, so a
// slow one means something is wrong; give up rather than delay the job.
const macOSBrewUntapTimeout = 60 * time.Second
