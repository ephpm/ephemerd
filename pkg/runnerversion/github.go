package runnerversion

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ephpm/ephemerd/pkg/runner"
	gh "github.com/google/go-github/v72/github"
)

// Candidate is one source's statement of the current runner for a platform.
type Candidate struct {
	Platform string
	Version  string
	Filename string
	URL      string
	SHA256   string
	// Source names who said so ("manifest", "github-runners-downloads").
	Source string
}

// Source resolves the current runner for a platform.
type Source interface {
	Resolve(ctx context.Context, platform string) (Candidate, error)
}

// ErrNoCandidate means the source had no entry for the platform.
var ErrNoCandidate = errors.New("no runner candidate for platform")

// GitHubSource resolves via GET /repos/{owner}/{repo}/actions/runners/downloads
// (or the /orgs/{org}/ form when Repo is empty). GitHub returns, per
// platform, download_url, filename and sha256_checksum — its own statement
// of the current runner's hash, which the upgrader cross-checks against
// the signed manifest. The call needs the same administration permission
// JIT registration already uses.
type GitHubSource struct {
	Client *gh.Client
	Owner  string
	// Repo is optional; empty uses the org endpoint.
	Repo string

	// list overrides the API call in tests.
	list func(ctx context.Context) ([]*gh.RunnerApplicationDownload, error)
}

// SourceName is the Candidate.Source value for this source.
const SourceGitHubDownloads = "github-runners-downloads"

// SourceManifest is the Candidate.Source value for the signed manifest.
const SourceManifest = "manifest"

// Resolve picks the entry whose os/architecture match platform.
func (g *GitHubSource) Resolve(ctx context.Context, platform string) (Candidate, error) {
	list := g.list
	if list == nil {
		list = g.apiList
	}
	downloads, err := list(ctx)
	if err != nil {
		return Candidate{}, fmt.Errorf("runners/downloads: %w", err)
	}
	os_, arch, ok := strings.Cut(platform, "-")
	if !ok {
		return Candidate{}, fmt.Errorf("%w: bad platform %q", ErrNoCandidate, platform)
	}
	for _, d := range downloads {
		if d.GetOS() != os_ || d.GetArchitecture() != arch {
			continue
		}
		c := Candidate{
			Platform: platform,
			Filename: d.GetFilename(),
			URL:      d.GetDownloadURL(),
			SHA256:   strings.ToLower(d.GetSHA256Checksum()),
			Source:   SourceGitHubDownloads,
		}
		parts := runner.ParseVersion(c.Filename)
		if len(parts) != 3 {
			return Candidate{}, fmt.Errorf("%w: cannot parse version from filename %q", ErrNoCandidate, c.Filename)
		}
		c.Version = fmt.Sprintf("%d.%d.%d", parts[0], parts[1], parts[2])
		if c.SHA256 == "" {
			return Candidate{}, fmt.Errorf("%w: GitHub returned no sha256_checksum for %s", ErrNoCandidate, c.Filename)
		}
		if !sha256Re.MatchString(c.SHA256) {
			return Candidate{}, fmt.Errorf("%w: sha256_checksum %q is not hex", ErrNoCandidate, c.SHA256)
		}
		return c, nil
	}
	return Candidate{}, fmt.Errorf("%w: %s", ErrNoCandidate, platform)
}

func (g *GitHubSource) apiList(ctx context.Context) ([]*gh.RunnerApplicationDownload, error) {
	if g.Client == nil {
		return nil, errors.New("github source has no client")
	}
	var (
		downloads []*gh.RunnerApplicationDownload
		resp      *gh.Response
		err       error
	)
	if g.Repo == "" {
		downloads, resp, err = g.Client.Actions.ListOrganizationRunnerApplicationDownloads(ctx, g.Owner)
	} else {
		downloads, resp, err = g.Client.Actions.ListRunnerApplicationDownloads(ctx, g.Owner, g.Repo)
	}
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("%s: %w", resp.Status, err)
		}
		return nil, err
	}
	return downloads, nil
}

// ManifestSource adapts a verified Manifest to the Source interface.
type ManifestSource struct{ Manifest *Manifest }

// Resolve returns the manifest's entry for platform.
func (m ManifestSource) Resolve(_ context.Context, platform string) (Candidate, error) {
	if m.Manifest == nil {
		return Candidate{}, ErrNoCandidate
	}
	a, err := m.Manifest.Artifact(platform)
	if err != nil {
		return Candidate{}, err
	}
	return Candidate{
		Platform: platform,
		Version:  m.Manifest.Version,
		Filename: ArchiveFilename(platform, m.Manifest.Version),
		URL:      a.URL,
		SHA256:   a.SHA256,
		Source:   SourceManifest,
	}, nil
}
