package diskspace

import (
	"math"
	"testing"
)

const gib = uint64(1024 * 1024 * 1024)

func TestUsage_Math(t *testing.T) {
	tests := []struct {
		name        string
		usage       Usage
		wantUsed    uint64
		wantUsedPct float64
		wantFreePct float64
	}{
		{
			name:        "half full",
			usage:       Usage{TotalBytes: 100 * gib, FreeBytes: 50 * gib},
			wantUsed:    50 * gib,
			wantUsedPct: 50,
			wantFreePct: 50,
		},
		{
			name:        "nearly full",
			usage:       Usage{TotalBytes: 116 * gib, FreeBytes: 6 * gib},
			wantUsed:    110 * gib,
			wantUsedPct: 94.83,
			wantFreePct: 5.17,
		},
		{
			name:        "empty disk",
			usage:       Usage{TotalBytes: 100 * gib, FreeBytes: 100 * gib},
			wantUsed:    0,
			wantUsedPct: 0,
			wantFreePct: 100,
		},
		{
			// A failed probe must not read as "100% used" or every node
			// would start garbage collecting on a syscall error.
			name:        "zero total reports zero, not a division artifact",
			usage:       Usage{TotalBytes: 0, FreeBytes: 0},
			wantUsed:    0,
			wantUsedPct: 0,
			wantFreePct: 0,
		},
		{
			// Free can exceed total on filesystems that over-report
			// (network mounts, sparse volumes). Clamp instead of
			// underflowing an unsigned subtraction.
			name:        "free above total clamps used to zero",
			usage:       Usage{TotalBytes: 10 * gib, FreeBytes: 20 * gib},
			wantUsed:    0,
			wantUsedPct: 0,
			wantFreePct: 200,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.UsedBytes(); got != tc.wantUsed {
				t.Errorf("UsedBytes() = %d, want %d", got, tc.wantUsed)
			}
			if got := tc.usage.UsedPercent(); math.Abs(got-tc.wantUsedPct) > 0.01 {
				t.Errorf("UsedPercent() = %.4f, want %.2f", got, tc.wantUsedPct)
			}
			if got := tc.usage.FreePercent(); math.Abs(got-tc.wantFreePct) > 0.01 {
				t.Errorf("FreePercent() = %.4f, want %.2f", got, tc.wantFreePct)
			}
		})
	}
}

func TestGiB(t *testing.T) {
	if got := GiB(2 * gib); got != 2 {
		t.Errorf("GiB(2GiB) = %v, want 2", got)
	}
	if got := GiB(0); got != 0 {
		t.Errorf("GiB(0) = %v, want 0", got)
	}
}

// TestCheck_RealFilesystem is the one test that touches a syscall. It only
// asserts the shape of the answer — capacity is non-zero and free does not
// exceed it — because the actual numbers belong to whatever machine is
// running the suite.
func TestCheck_RealFilesystem(t *testing.T) {
	u, err := Check(t.TempDir())
	if err != nil {
		t.Skipf("Check unsupported or failed on this platform: %v", err)
	}
	if u.TotalBytes == 0 {
		t.Fatal("TotalBytes = 0 on a real filesystem")
	}
	if u.FreeBytes > u.TotalBytes {
		t.Fatalf("FreeBytes %d exceeds TotalBytes %d", u.FreeBytes, u.TotalBytes)
	}
	if pct := u.UsedPercent(); pct < 0 || pct > 100 {
		t.Fatalf("UsedPercent() = %v, out of range", pct)
	}
	if u.String() == "" {
		t.Error("String() is empty")
	}
}
