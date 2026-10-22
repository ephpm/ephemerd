package runner

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf16"
)

// The GitHub runner exports the cache/artifact service address to every
// step as ACTIONS_RESULTS_URL, taken from the job message. It sets it on the
// step's own environment, so it overwrites any value the container was
// started with — which is why ephemerd cannot simply point jobs at its LAN
// cache with an env var.
//
// PatchResultsURL renames that one string inside Runner.Worker.dll to a name
// of the same length. The runner then exports GitHub's address under the
// unused name, and the ACTIONS_RESULTS_URL ephemerd put in the container's
// environment is what the step sees. This is the technique
// falcondev-oss/github-actions-cache-server documents; the rename has to be
// same-length because the string lives in the assembly's #US heap, whose
// entries are length-prefixed.
//
// Verified against runner 2.337.0 (linux-x64 tarball and the win-x64 zip
// embedded here): Runner.Worker.dll holds exactly ONE UTF-16LE occurrence
// and Runner.Common.dll/Runner.Sdk.dll hold none.
const (
	// ResultsURLVar is the variable the unpatched runner exports.
	ResultsURLVar = "ACTIONS_RESULTS_URL"
	// ResultsURLRenamed is what a patched runner exports GitHub's address
	// as instead. Same length as ResultsURLVar — see above.
	ResultsURLRenamed = "ACTIONS_RESULTS_ORL"
)

// WorkerDLL is the runner assembly that holds the string, relative to the
// runner root.
var WorkerDLL = filepath.Join("bin", "Runner.Worker.dll")

var (
	resultsURLUTF16     = utf16LE(ResultsURLVar)
	resultsRenamedUTF16 = utf16LE(ResultsURLRenamed)
)

func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(units)*2)
	for _, u := range units {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

// PatchResultsURL rewrites every UTF-16LE occurrence of ResultsURLVar in data
// in place. It reports whether data now exports the renamed variable: true
// when it patched at least one occurrence or data was already patched, false
// when neither name is present (a runner version that no longer reads the
// variable from this assembly — the caller must then leave the cache off).
func PatchResultsURL(data []byte) (patched bool) {
	n := 0
	for {
		i := bytes.Index(data, resultsURLUTF16)
		if i < 0 {
			break
		}
		copy(data[i:], resultsRenamedUTF16)
		n++
	}
	if n > 0 {
		return true
	}
	return bytes.Contains(data, resultsRenamedUTF16)
}

// PatchResultsURLFile patches the Runner.Worker.dll under runnerRoot.
//
// When writableRoot is non-empty and differs from runnerRoot, the patched
// copy is written to writableRoot (an overlay upperdir, or a per-job copy)
// and runnerRoot is never modified — the shared runner tree must stay
// byte-identical for jobs that do not use the LAN cache. Otherwise the file
// is rewritten in place, keeping its mode.
//
// A missing assembly or an absent string is not an error: it returns false
// and the caller simply does not turn the cache on for the job.
func PatchResultsURLFile(runnerRoot, writableRoot string) (bool, error) {
	src := filepath.Join(runnerRoot, WorkerDLL)
	dst := src
	if writableRoot != "" && writableRoot != runnerRoot {
		dst = filepath.Join(writableRoot, WorkerDLL)
		// An earlier patch of this same writable tree wins: it may already
		// be the only copy that exists (overlay upper over a lower that was
		// never touched).
		if _, err := os.Stat(dst); err == nil {
			src = dst
		}
	}
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", src, err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", src, err)
	}
	if !PatchResultsURL(data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, fmt.Errorf("creating %s: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return false, fmt.Errorf("writing %s: %w", dst, err)
	}
	return true, nil
}
