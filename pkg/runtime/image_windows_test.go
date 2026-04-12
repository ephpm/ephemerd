//go:build windows

package runtime

import (
	"strings"
	"testing"
)

func TestBuildToTag(t *testing.T) {
	tests := []struct {
		name  string
		major uint32
		minor uint32
		build uint32
		want  string
	}{
		{"Windows Server 2025", 10, 0, 26100, "ltsc2025"},
		{"Windows 11 24H2", 10, 0, 26200, "ltsc2025"},
		{"Windows Server 2022", 10, 0, 20348, "ltsc2022"},
		{"Windows Server 2022 update", 10, 0, 25000, "ltsc2022"},
		{"Windows Server 2019", 10, 0, 17763, "ltsc2019"},
		{"Windows Server 2019 update", 10, 0, 19041, "ltsc2019"},
		{"Unknown old build", 10, 0, 15000, "ltsc2025"},
		{"Future build", 10, 0, 30000, "ltsc2025"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildToTag(tt.major, tt.minor, tt.build)
			if got != tt.want {
				t.Errorf("buildToTag(%d, %d, %d) = %q, want %q", tt.major, tt.minor, tt.build, got, tt.want)
			}
		})
	}
}

func TestDefaultImage_Windows(t *testing.T) {
	img := defaultImage()
	if !strings.Contains(img, "servercore") {
		t.Errorf("defaultImage() = %q, expected servercore on Windows", img)
	}
	if !strings.Contains(img, "ltsc") {
		t.Errorf("defaultImage() = %q, expected ltsc tag", img)
	}
}
