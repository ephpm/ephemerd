package workflow

import "strings"

// TargetPlatform identifies the OS a workflow job targets.
type TargetPlatform int

const (
	PlatformLinux   TargetPlatform = iota
	PlatformWindows
	PlatformMacOS
)

func (p TargetPlatform) String() string {
	switch p {
	case PlatformLinux:
		return "linux"
	case PlatformWindows:
		return "windows"
	case PlatformMacOS:
		return "macos"
	default:
		return "unknown"
	}
}

// DetectPlatform determines the target OS from a job's runs-on field.
// The runs-on value can be a string ("ubuntu-latest") or a list
// (["self-hosted", "linux", "x64"]). If no OS label is found, linux
// is assumed (matching GitHub Actions default behavior).
func DetectPlatform(runsOn interface{}) TargetPlatform {
	labels := normalizeRunsOn(runsOn)
	for _, label := range labels {
		label = strings.ToLower(label)
		switch {
		case label == "linux" || strings.HasPrefix(label, "ubuntu-"):
			return PlatformLinux
		case label == "windows" || strings.HasPrefix(label, "windows-"):
			return PlatformWindows
		case label == "macos" || label == "macosx" || strings.HasPrefix(label, "macos-"):
			return PlatformMacOS
		}
	}
	return PlatformLinux
}

// normalizeRunsOn converts the runs-on field (string, []string, or
// []interface{}) to a []string.
func normalizeRunsOn(v interface{}) []string {
	switch val := v.(type) {
	case string:
		return []string{val}
	case []string:
		return val
	case []interface{}:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
