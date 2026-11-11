package runnerversion

import (
	"context"
	"errors"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func ghDownloads(version, sha string) []*gh.RunnerApplicationDownload {
	mk := func(os_, arch string) *gh.RunnerApplicationDownload {
		name := ArchiveFilename(os_+"-"+arch, version)
		return &gh.RunnerApplicationDownload{
			OS:             gh.Ptr(os_),
			Architecture:   gh.Ptr(arch),
			Filename:       gh.Ptr(name),
			DownloadURL:    gh.Ptr("https://github.com/actions/runner/releases/download/v" + version + "/" + name),
			SHA256Checksum: gh.Ptr(sha),
		}
	}
	return []*gh.RunnerApplicationDownload{mk("linux", "x64"), mk("linux", "arm64"), mk("win", "x64"), mk("osx", "arm64")}
}

func TestGitHubSourceResolve(t *testing.T) {
	src := &GitHubSource{list: func(context.Context) ([]*gh.RunnerApplicationDownload, error) {
		return ghDownloads("2.338.2", zeroSHA), nil
	}}
	c, err := src.Resolve(context.Background(), "win-x64")
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "2.338.2" || c.SHA256 != zeroSHA || c.Filename != "actions-runner-win-x64-2.338.2.zip" || c.Source != SourceGitHubDownloads {
		t.Fatalf("candidate %+v", c)
	}
	if _, err := src.Resolve(context.Background(), "osx-x64"); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("missing platform: %v", err)
	}
}

func TestGitHubSourceRejectsMissingChecksum(t *testing.T) {
	src := &GitHubSource{list: func(context.Context) ([]*gh.RunnerApplicationDownload, error) {
		d := ghDownloads("2.338.2", zeroSHA)
		d[0].SHA256Checksum = nil
		return d, nil
	}}
	if _, err := src.Resolve(context.Background(), "linux-x64"); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("expected ErrNoCandidate without checksum, got %v", err)
	}
	src = &GitHubSource{list: func(context.Context) ([]*gh.RunnerApplicationDownload, error) {
		return nil, errors.New("boom")
	}}
	if _, err := src.Resolve(context.Background(), "linux-x64"); err == nil {
		t.Fatal("expected api error")
	}
}

func TestManifestSource(t *testing.T) {
	m := validManifest("2.338.2", "2.338.0", zeroSHA)
	c, err := ManifestSource{Manifest: m}.Resolve(context.Background(), "linux-x64")
	if err != nil || c.Version != "2.338.2" || c.Source != SourceManifest || c.URL != m.Platforms["linux-x64"].URL {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := (ManifestSource{}).Resolve(context.Background(), "linux-x64"); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("nil manifest: %v", err)
	}
}
