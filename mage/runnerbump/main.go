// Command runnerbump bumps ephemerd's embedded GitHub Actions runner to the
// newest actions/runner release.
//
// It is the engine behind .github/workflows/runner-bump.yml, and is equally
// usable by hand:
//
//	go run ./mage/runnerbump                   # bump to the latest stable release
//	go run ./mage/runnerbump -version 2.338.0  # bump to a specific release
//	go run ./mage/runnerbump -check            # report only, edit nothing
//
// What a bump touches (the shape of ephpm/ephemerd#183):
//
//   - mage/download/download.go: the RunnerVersion const, the
//     "// GitHub Actions runner (actions/runner vX)" comment, and the five
//     actions-runner-* archive entries of pinnedSHA256.
//   - images/runner-ci-{linux,macos,windows}/Dockerfile: ARG RUNNER_VERSION.
//
// Checksums come from the release API's per-asset "digest" field (computed by
// GitHub over the uploaded bytes). An asset without a digest is downloaded and
// hashed instead. The release notes' "BEGIN SHA" markers are only ever used as
// a cross-check: a disagreement fails the bump, but the notes are never the
// sole source of a pin. Any of the five archives missing from the release
// fails the bump.
//
// A target that is not newer than the current RunnerVersion is a no-op, so the
// command is safe to run on a schedule.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Paths, relative to -root, that a bump edits.
const downloadGo = "mage/download/download.go"

var dockerfiles = []string{
	"images/runner-ci-linux/Dockerfile",
	"images/runner-ci-macos/Dockerfile",
	"images/runner-ci-windows/Dockerfile",
}

// runnerPlatforms are the archives ephemerd pins, in pinnedSHA256 order. The
// name stem doubles as the release notes' "BEGIN SHA <stem>" marker.
var runnerPlatforms = []struct{ stem, ext string }{
	{"linux-x64", "tar.gz"},
	{"linux-arm64", "tar.gz"},
	{"win-x64", "zip"},
	{"osx-x64", "tar.gz"},
	{"osx-arm64", "tar.gz"},
}

func assetName(stem, ext, version string) string {
	return fmt.Sprintf("actions-runner-%s-%s.%s", stem, version, ext)
}

const repo = "actions/runner"

// release is the subset of the GitHub release object runnerbump reads.
type release struct {
	TagName    string  `json:"tag_name"`
	HTMLURL    string  `json:"html_url"`
	Body       string  `json:"body"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []asset `json:"assets"`
}

type asset struct {
	Name               string `json:"name"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// pin is one resolved archive checksum.
type pin struct {
	Name   string
	SHA256 string
	Source string // "release API digest" or "downloaded and hashed"
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runnerbump:", err)
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("root", ".", "repository root to edit")
	target := flag.String("version", "", "runner version to bump to (default: latest stable actions/runner release)")
	check := flag.Bool("check", false, "report whether a bump is available without editing anything")
	releaseJSON := flag.String("release-json", "", "read the release object from this file instead of the GitHub API (testing)")
	bodyOut := flag.String("body-out", "", "write a markdown PR body to this file when a bump happens")
	flag.Parse()

	dlPath := filepath.Join(*root, downloadGo)
	src, err := os.ReadFile(dlPath)
	if err != nil {
		return err
	}
	current, err := currentVersion(src)
	if err != nil {
		return err
	}

	rel, err := loadRelease(*releaseJSON, *target)
	if err != nil {
		return err
	}
	if rel.Draft || rel.Prerelease {
		return fmt.Errorf("release %s is a draft or prerelease; refusing to pin it", rel.TagName)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	if _, err := parseSemver(latest); err != nil {
		return fmt.Errorf("release tag %q: %w", rel.TagName, err)
	}
	newer, err := semverLess(current, latest)
	if err != nil {
		return err
	}

	fmt.Printf("current runner: %s\nrelease:        %s (%s)\n", current, latest, rel.HTMLURL)
	if !newer {
		fmt.Println("up to date; nothing to do")
		return writeOutputs(map[string]string{"bump": "false", "current": current, "latest": latest})
	}
	if *check {
		fmt.Println("bump available (-check: not editing)")
		return writeOutputs(map[string]string{"bump": "false", "available": "true", "current": current, "latest": latest})
	}

	pins, err := resolvePins(rel, latest)
	if err != nil {
		return err
	}

	out, err := rewriteDownloadGo(src, current, latest, pins)
	if err != nil {
		return fmt.Errorf("%s: %w", downloadGo, err)
	}
	if err := writeFileKeepMode(dlPath, out); err != nil {
		return err
	}
	for _, df := range dockerfiles {
		p := filepath.Join(*root, df)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		nb, err := rewriteDockerfile(b, latest)
		if err != nil {
			return fmt.Errorf("%s: %w", df, err)
		}
		if err := writeFileKeepMode(p, nb); err != nil {
			return err
		}
	}

	fmt.Printf("bumped %s -> %s\n", current, latest)
	for _, p := range pins {
		fmt.Printf("  %-45s %s (%s)\n", p.Name, p.SHA256, p.Source)
	}
	if *bodyOut != "" {
		if err := os.WriteFile(*bodyOut, []byte(prBody(current, latest, rel.HTMLURL, pins)), 0o644); err != nil {
			return err
		}
	}
	return writeOutputs(map[string]string{"bump": "true", "current": current, "latest": latest})
}

// ---------------------------------------------------------------------------
// Versions

var runnerVersionRe = regexp.MustCompile(`(?m)^\s*RunnerVersion\s*=\s*"([^"]+)"`)

func currentVersion(src []byte) (string, error) {
	m := runnerVersionRe.FindAllSubmatch(src, -1)
	if len(m) != 1 {
		return "", fmt.Errorf("expected exactly one RunnerVersion const in %s, found %d", downloadGo, len(m))
	}
	v := string(m[0][1])
	if _, err := parseSemver(v); err != nil {
		return "", fmt.Errorf("RunnerVersion %q: %w", v, err)
	}
	return v, nil
}

func parseSemver(v string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("not a MAJOR.MINOR.PATCH version")
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return out, fmt.Errorf("not a MAJOR.MINOR.PATCH version")
		}
		out[i] = n
	}
	return out, nil
}

// semverLess reports whether a < b.
func semverLess(a, b string) (bool, error) {
	va, err := parseSemver(a)
	if err != nil {
		return false, fmt.Errorf("%q: %w", a, err)
	}
	vb, err := parseSemver(b)
	if err != nil {
		return false, fmt.Errorf("%q: %w", b, err)
	}
	for i := range va {
		if va[i] != vb[i] {
			return va[i] < vb[i], nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// Release lookup and checksums

var httpClient = &http.Client{Timeout: 10 * time.Minute}

func loadRelease(file, version string) (*release, error) {
	var data []byte
	var err error
	if file != "" {
		data, err = os.ReadFile(file)
	} else {
		// /releases/latest only ever returns a published, non-prerelease,
		// non-draft release; run() re-checks both flags regardless.
		path := "releases/latest"
		if version != "" {
			path = "releases/tags/v" + strings.TrimPrefix(version, "v")
		}
		data, err = apiGet("https://api.github.com/repos/" + repo + "/" + path)
	}
	if err != nil {
		return nil, err
	}
	var rel release
	if err := json.Unmarshal(data, &rel); err != nil {
		return nil, fmt.Errorf("decoding release: %w", err)
	}
	if rel.TagName == "" {
		return nil, errors.New("release has no tag_name")
	}
	return &rel, nil
}

func apiGet(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if t := os.Getenv(k); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
			break
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func resolvePins(rel *release, version string) ([]pin, error) {
	byName := make(map[string]asset, len(rel.Assets))
	for _, a := range rel.Assets {
		byName[a.Name] = a
	}
	var pins []pin
	var missing []string
	for _, p := range runnerPlatforms {
		name := assetName(p.stem, p.ext, version)
		a, ok := byName[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		sum, source, err := assetSHA256(a)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if noted, ok := notesSHA256(rel.Body, p.stem); ok && noted != sum {
			return nil, fmt.Errorf("%s: release-notes SHA %s disagrees with %s %s; refusing to pin", name, noted, source, sum)
		}
		pins = append(pins, pin{Name: name, SHA256: sum, Source: source})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("release %s is missing required assets: %s", rel.TagName, strings.Join(missing, ", "))
	}
	return pins, nil
}

func assetSHA256(a asset) (sum, source string, err error) {
	if d, ok := strings.CutPrefix(a.Digest, "sha256:"); ok {
		d = strings.ToLower(d)
		if !hex64.MatchString(d) {
			return "", "", fmt.Errorf("malformed digest %q", a.Digest)
		}
		return d, "release API digest", nil
	}
	if a.Digest != "" {
		return "", "", fmt.Errorf("unsupported digest algorithm %q", a.Digest)
	}
	if a.BrowserDownloadURL == "" {
		return "", "", errors.New("no digest and no download URL")
	}
	resp, err := httpClient.Get(a.BrowserDownloadURL)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GET %s: %s", a.BrowserDownloadURL, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", "", fmt.Errorf("downloading %s: %w", a.BrowserDownloadURL, err)
	}
	return hex.EncodeToString(h.Sum(nil)), "downloaded and hashed", nil
}

// notesSHA256 extracts the checksum actions/runner embeds in its release notes
// as <!-- BEGIN SHA linux-x64 -->HASH<!-- END SHA linux-x64 -->.
func notesSHA256(body, stem string) (string, bool) {
	re := regexp.MustCompile(`<!--\s*BEGIN SHA ` + regexp.QuoteMeta(stem) + `\s*-->\s*([0-9a-fA-F]{64})\s*<!--\s*END SHA`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

// ---------------------------------------------------------------------------
// Edits

// rewriteDownloadGo moves every runner pin site in download.go from old to
// version. Each site must match exactly once, and no trace of old may survive,
// so a refactor of download.go fails the bump loudly instead of producing a
// half-edited file.
func rewriteDownloadGo(src []byte, old, version string, pins []pin) ([]byte, error) {
	s := string(src)
	qOld := regexp.QuoteMeta(old)

	replaceOne := func(what string, re *regexp.Regexp, repl string) error {
		n := len(re.FindAllStringIndex(s, -1))
		if n != 1 {
			return fmt.Errorf("expected exactly one %s, found %d", what, n)
		}
		s = re.ReplaceAllString(s, repl)
		return nil
	}

	if err := replaceOne("RunnerVersion const",
		regexp.MustCompile(`(?m)^(\s*RunnerVersion\s*=\s*)"`+qOld+`"`),
		`${1}"`+version+`"`); err != nil {
		return nil, err
	}
	if err := replaceOne("actions/runner pin comment",
		regexp.MustCompile(`(?m)^(\s*// GitHub Actions runner \(actions/runner v)`+qOld+`\)`),
		`${1}`+version+`)`); err != nil {
		return nil, err
	}
	for i, p := range runnerPlatforms {
		oldName := regexp.QuoteMeta(assetName(p.stem, p.ext, old))
		if err := replaceOne("pinnedSHA256 entry for "+assetName(p.stem, p.ext, old),
			regexp.MustCompile(`(?m)^(\s*)"`+oldName+`":\s*"[0-9a-f]{64}",`),
			`${1}"`+pins[i].Name+`": "`+pins[i].SHA256+`",`); err != nil {
			return nil, err
		}
	}
	if strings.Contains(s, old) {
		return nil, fmt.Errorf("old version %s still appears after the rewrite; a new pin site needs teaching to runnerbump", old)
	}

	// gofmt realigns the map values the replacements disturbed.
	out, err := format.Source([]byte(s))
	if err != nil {
		return nil, fmt.Errorf("gofmt after rewrite: %w", err)
	}
	return out, nil
}

var dockerArgRe = regexp.MustCompile(`(?m)^ARG RUNNER_VERSION=\S+`)

// rewriteDockerfile sets ARG RUNNER_VERSION to version whatever it was before,
// so a CI image that drifted from download.go is pulled back into step.
func rewriteDockerfile(src []byte, version string) ([]byte, error) {
	if n := len(dockerArgRe.FindAllIndex(src, -1)); n != 1 {
		return nil, fmt.Errorf("expected exactly one ARG RUNNER_VERSION line, found %d", n)
	}
	return dockerArgRe.ReplaceAll(src, []byte("ARG RUNNER_VERSION="+version)), nil
}

func writeFileKeepMode(path string, data []byte) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, fi.Mode().Perm())
}

// ---------------------------------------------------------------------------
// Workflow plumbing

// writeOutputs appends key=value lines to $GITHUB_OUTPUT when running in
// Actions, and is a no-op otherwise.
func writeOutputs(kv map[string]string) error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	for _, k := range []string{"bump", "available", "current", "latest"} {
		if v, ok := kv[k]; ok {
			if _, err := fmt.Fprintf(f, "%s=%s\n", k, v); err != nil {
				_ = f.Close()
				return err
			}
		}
	}
	return f.Close()
}

func prBody(old, version, notesURL string, pins []pin) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Automated bump of ephemerd's embedded GitHub Actions runner: `%s` -> `%s`.\n\n", old, version)
	fmt.Fprintf(&b, "Upstream release notes: %s\n\n", notesURL)
	b.WriteString("GitHub deprecates old runner versions and then refuses their connections, which silently stalls every job on that pool, so this should not sit for long.\n\n")
	b.WriteString("| Archive | SHA-256 | Source |\n|---|---|---|\n")
	for _, p := range pins {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", p.Name, p.SHA256, p.Source)
	}
	b.WriteString("\nChecksums come from the release API's per-asset `digest` field (or a download-and-hash when a digest is absent), cross-checked against the release notes' `BEGIN SHA` markers where present.\n\n")
	b.WriteString("Edited: `RunnerVersion` and the five runner `pinnedSHA256` entries in `mage/download/download.go`, and `ARG RUNNER_VERSION` in `images/runner-ci-{linux,macos,windows}/Dockerfile`.\n\n")
	b.WriteString("Merging changes `mage/download/download.go`, which rebuilds the CI runner images (`build-images.yml`).\n\n")
	b.WriteString("<sub>Opened by `.github/workflows/runner-bump.yml` (`go run ./mage/runnerbump`). Reruns update this PR in place; it is closed automatically if main catches up some other way.</sub>\n")
	return b.String()
}
