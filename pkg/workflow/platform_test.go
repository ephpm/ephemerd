package workflow

import "testing"

func TestDetectPlatform(t *testing.T) {
	tests := []struct {
		name   string
		runsOn interface{}
		want   TargetPlatform
	}{
		{"ubuntu-latest string", "ubuntu-latest", PlatformLinux},
		{"ubuntu-22.04 string", "ubuntu-22.04", PlatformLinux},
		{"linux string", "linux", PlatformLinux},
		{"windows-latest string", "windows-latest", PlatformWindows},
		{"windows-2022 string", "windows-2022", PlatformWindows},
		{"windows string", "windows", PlatformWindows},
		{"macos-14 string", "macos-14", PlatformMacOS},
		{"macos-latest string", "macos-latest", PlatformMacOS},
		{"macos string", "macos", PlatformMacOS},
		{"macosx string", "macosx", PlatformMacOS},
		{
			"self-hosted linux list ([]interface{})",
			[]interface{}{"self-hosted", "linux", "x64"},
			PlatformLinux,
		},
		{
			"self-hosted windows list ([]interface{})",
			[]interface{}{"self-hosted", "windows", "x64"},
			PlatformWindows,
		},
		{
			"self-hosted only ([]interface{})",
			[]interface{}{"self-hosted"},
			PlatformLinux, // default
		},
		{
			"[]string with linux",
			[]string{"self-hosted", "linux"},
			PlatformLinux,
		},
		{
			"[]string with macos",
			[]string{"self-hosted", "macos"},
			PlatformMacOS,
		},
		{"nil runs-on", nil, PlatformLinux},
		{"empty string", "", PlatformLinux},
		{"unknown label", "custom-runner", PlatformLinux},
		{"case insensitive", "Ubuntu-Latest", PlatformLinux},
		{"windows case insensitive", "Windows-2022", PlatformWindows},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectPlatform(tt.runsOn)
			if got != tt.want {
				t.Errorf("DetectPlatform(%v) = %v, want %v", tt.runsOn, got, tt.want)
			}
		})
	}
}
