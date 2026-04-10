package vm

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
)

// RunDistro tracks a single ephemerd-run WSL distro instance.
// Each concurrent "ephemerd run" gets its own distro with a unique name.
type RunDistro struct {
	Name    string
	dataDir string
	log     *slog.Logger
}

// RunDistroConfig configures the creation of a new RunDistro.
type RunDistroConfig struct {
	DataDir string
	Log     *slog.Logger
}

// RunInWSLConfig configures a single ephemerd run delegation to WSL.
type RunInWSLConfig struct {
	WorkflowPath string // absolute path to the workflow YAML
	JobFilter    string // optional --job filter
	RepoDir      string // absolute path to the repo root
}

// generateDistroName returns a unique WSL distro name with the given prefix
// and an 8-character hex suffix from crypto/rand.
// Example: "ephemerd-run-a1b2c3d4"
func generateDistroName(prefix string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// WindowsPathToWSL converts a Windows absolute path to a WSL /mnt/ path.
// Example: C:\Users\luthe\repo → /mnt/c/Users/luthe/repo
func WindowsPathToWSL(winPath string) (string, error) {
	if len(winPath) < 2 {
		return "", fmt.Errorf("path too short: %q", winPath)
	}

	// Reject UNC paths (\\server\share)
	if strings.HasPrefix(winPath, `\\`) {
		return "", fmt.Errorf("UNC paths are not supported: %q", winPath)
	}

	// Require drive letter (e.g. C:)
	drive := rune(winPath[0])
	if !unicode.IsLetter(drive) || winPath[1] != ':' {
		return "", fmt.Errorf("expected drive letter path, got: %q", winPath)
	}

	rest := winPath[2:]
	rest = strings.ReplaceAll(rest, `\`, "/")

	return fmt.Sprintf("/mnt/%c%s", unicode.ToLower(drive), rest), nil
}
