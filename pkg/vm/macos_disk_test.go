package vm

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// baseImageBytes is the fleet's Tart base image: 50,000,000,000 bytes, ~46.6 GiB.
const baseImageBytes = 50_000_000_000

// TestMacOSDiskGrowBytes pins the truncate decision: grow only when the
// target is strictly larger than the clone, never shrink.
func TestMacOSDiskGrowBytes(t *testing.T) {
	tests := []struct {
		name   string
		cur    int64
		wantGB uint64
		size   int64
		grow   bool
	}{
		{"unset keeps the base size", baseImageBytes, 0, baseImageBytes, false},
		{"100 GiB grows", baseImageBytes, 100, 100 * gib, true},
		{"smaller than base never shrinks", baseImageBytes, 40, baseImageBytes, false},
		{"46 GiB is still below 50e9 bytes", baseImageBytes, 46, baseImageBytes, false},
		{"exactly the current size is not a growth", 100 * gib, 100, 100 * gib, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, grow := macOSDiskGrowBytes(tt.cur, tt.wantGB)
			if size != tt.size || grow != tt.grow {
				t.Errorf("macOSDiskGrowBytes(%d, %d) = (%d, %v), want (%d, %v)", tt.cur, tt.wantGB, size, grow, tt.size, tt.grow)
			}
		})
	}
}

func TestResolveMacOSDiskSize(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base.img")
	f, err := os.Create(base)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse on every filesystem the tests run on; nothing is written.
	if err := f.Truncate(baseImageBytes); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if got := ResolveMacOSDiskSize(base, 0, discardLog()); got != 0 {
		t.Errorf("unset: got %d, want 0 (base image size, behaviour unchanged)", got)
	}
	if got := ResolveMacOSDiskSize(base, 100, discardLog()); got != 100 {
		t.Errorf("100 GiB: got %d, want 100", got)
	}
	if got := ResolveMacOSDiskSize(base, 30, discardLog()); got != 0 {
		t.Errorf("smaller than the base image: got %d, want 0 (ignored, never shrunk)", got)
	}
	if got := ResolveMacOSDiskSize(filepath.Join(t.TempDir(), "missing.img"), 100, discardLog()); got != 0 {
		t.Errorf("unreadable base image: got %d, want 0", got)
	}
}

// diskutilInfoData is `diskutil info /System/Volumes/Data` from a guest booted
// off the fleet's base image (layout verified 2026-10-22: disk0s1 ISC, disk0s2
// the APFS container, then free space). Trimmed to the lines around the one
// the parser needs, in diskutil's real alignment.
const diskutilInfoData = `   Device Identifier:         disk3s5
   Device Node:               /dev/disk3s5
   Whole:                     No
   Part of Whole:             disk3

   Volume Name:               Data
   Mounted:                   Yes
   Mount Point:               /System/Volumes/Data

   Partition Type:            41504653-0000-11AA-AA11-00306543ECAC
   File System Personality:   APFS
   Type (Bundle):             apfs
   Name (User Visible):       APFS

   APFS Container:            disk3
   APFS Physical Store:       disk0s2
   Fusion Drive:              No
   APFS Volume Group:         0C6E6B2A-7D7B-4C59-9E77-2C7D64A8E3E1
   Encrypted:                 No
`

func TestParseAPFSPhysicalStore(t *testing.T) {
	store, whole, err := parseAPFSPhysicalStore(diskutilInfoData)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if store != "disk0s2" || whole != "disk0" {
		t.Errorf("got store=%q whole=%q, want disk0s2 / disk0", store, whole)
	}

	// Multi-digit identifiers must split on the LAST "s".
	store, whole, err = parseAPFSPhysicalStore("   APFS Physical Store:       disk12s10\n")
	if err != nil || store != "disk12s10" || whole != "disk12" {
		t.Errorf("disk12s10: got (%q, %q, %v), want (disk12s10, disk12, nil)", store, whole, err)
	}
}

func TestParseAPFSPhysicalStore_Refuses(t *testing.T) {
	for name, info := range map[string]string{
		"no store line":    "   Device Identifier:   disk3s5\n   File System Personality:   APFS\n",
		"whole disk":       "   APFS Physical Store:   disk0\n",
		"shell metachars":  "   APFS Physical Store:   disk0s2; rm -rf /\n",
		"empty value":      "   APFS Physical Store:\n",
		"not a disk ident": "   APFS Physical Store:   /dev/disk0s2\n",
	} {
		t.Run(name, func(t *testing.T) {
			if store, _, err := parseAPFSPhysicalStore(info); err == nil {
				t.Errorf("accepted %q; anything but a plain diskNsM must be refused", store)
			}
		})
	}
}

// TestMacOSDiskGrowScript checks the guest commands target the parsed disk
// and never fail silently: repairDisk is fed its confirmation, sudo is
// non-interactive (a prompt would hang until the deadline), and the status
// line the caller matches on is always printed.
func TestMacOSDiskGrowScript(t *testing.T) {
	s := macOSDiskGrowScript("disk0", "disk0s2")
	for _, want := range []string{
		"yes | sudo -n diskutil repairDisk disk0 ",
		"sudo -n diskutil apfs resizeContainer disk0s2 0 ",
		"r=ok",
		`echo "before=${before}G after=${after}G resize=${r}"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("grow script missing %q:\n%s", want, s)
		}
	}
}

// TestMacOSGuestTuning_Bounded: the guest-side steps run inside the macOS
// provisioning deadline (5m by default), so a wedged diskutil or brew can
// only cost the job its extra disk or a warning — never the job.
func TestMacOSGuestTuning_Bounded(t *testing.T) {
	const provisionDeadline = 5 * time.Minute
	// info + grow + untap, all before the runner starts.
	if total := 90*time.Second + macOSDiskGrowTimeout + macOSBrewUntapTimeout; total >= provisionDeadline {
		t.Errorf("guest tuning can take %s, not inside the %s provisioning deadline", total, provisionDeadline)
	}
	if !strings.Contains(macOSDiskInfoCmd, "diskutil info /System/Volumes/Data") || !strings.Contains(macOSDiskInfoCmd, "|| diskutil info /") {
		t.Errorf("info command must ask the Data volume and fall back to /: %q", macOSDiskInfoCmd)
	}
}

// TestMacOSBrewUntapScript: brew runs as the guest's admin user (it refuses
// root, and the tap belongs to admin), never auto-updates (that would make
// every job reach the network for a cosmetic step), and only the buildkite
// tap is touched.
func TestMacOSBrewUntapScript(t *testing.T) {
	if strings.Contains(macOSBrewUntapScript, "sudo") {
		t.Error("brew must not run as root")
	}
	for _, want := range []string{
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"grep -qx buildkite/buildkite",
		`untap --force buildkite/buildkite`,
		"echo no-brew; exit 0",
	} {
		if !strings.Contains(macOSBrewUntapScript, want) {
			t.Errorf("untap script missing %q", want)
		}
	}
}

