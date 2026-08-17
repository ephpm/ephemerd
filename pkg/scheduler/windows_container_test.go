package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// resolveImageWithLog runs resolveImage against a mock provider whose
// FetchJobImage returns workflowImage (empty = the job declared no
// `container:`), and returns both the resolved image and everything the
// scheduler logged while resolving it.
func resolveImageWithLog(t *testing.T, workflowImage, jobOS string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	p := newMockProvider("github")
	if workflowImage != "" {
		p.images[7] = workflowImage
	}
	s := New(Config{
		Providers: []providers.Provider{p},
		Log:       slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	event := providers.JobEvent{Provider: p, JobID: 7, Repo: "acme/widgets"}
	return s.resolveImage(context.Background(), &event, jobOS), buf.String()
}

// A Windows job that declares `container:` is the combination ephemerd cannot
// serve end to end: it uses the image as the runner image, and the Actions
// runner then asks for container operations that pkg/dind refuses on Windows
// (checkWindowsSiblingGate). The job dies in "Set up job" with a bare
// "docker: command not found", so the daemon log is the only place the real
// reason can appear — this asserts it does.
func TestResolveImage_WarnsOnWindowsContainerJob(t *testing.T) {
	img, logged := resolveImageWithLog(t, "golang:1.26.6-windowsservercore-ltsc2025", "windows")

	if img != "golang:1.26.6-windowsservercore-ltsc2025" {
		t.Fatalf("resolveImage() = %q; the image must still be honoured, only warned about", img)
	}
	if !strings.Contains(logged, "container: on a Windows runner") {
		t.Fatalf("no warning logged for a Windows container: job; log=%q", logged)
	}
	for _, want := range []string{"acme/widgets", "workaround", "runner.images"} {
		if !strings.Contains(logged, want) {
			t.Errorf("warning omits %q; log=%q", want, logged)
		}
	}
}

// The warning is specific to the `container:` + Windows pair. A Windows job
// whose image came from [runner.images.<repo>].windows or from the provider
// default is the supported path and must stay quiet, or the warning becomes
// noise every operator learns to ignore.
func TestResolveImage_QuietWhenWindowsImageIsNotFromContainerDirective(t *testing.T) {
	var buf bytes.Buffer
	p := newMockProvider("github")
	s := New(Config{
		Providers: []providers.Provider{p},
		Log:       slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		RunnerImageForRepo: func(repo, os string) string {
			if repo == "acme/widgets" && os == "windows" {
				return "ghcr.io/ephpm/ephemerd:runner-ci-windows"
			}
			return ""
		},
	})
	event := providers.JobEvent{Provider: p, JobID: 7, Repo: "acme/widgets"}

	img := s.resolveImage(context.Background(), &event, "windows")
	if img != "ghcr.io/ephpm/ephemerd:runner-ci-windows" {
		t.Fatalf("resolveImage() = %q, want the per-repo override", img)
	}
	if buf.Len() != 0 {
		t.Errorf("per-repo Windows override warned; log=%q", buf.String())
	}
}

// `container:` on Linux and macOS is the path that works, and stays silent.
func TestResolveImage_QuietForContainerJobsOnOtherOSes(t *testing.T) {
	for _, jobOS := range []string{"linux", "macos"} {
		t.Run(jobOS, func(t *testing.T) {
			img, logged := resolveImageWithLog(t, "golang:1.26.6-bookworm", jobOS)
			if img != "golang:1.26.6-bookworm" {
				t.Fatalf("resolveImage() = %q", img)
			}
			if logged != "" {
				t.Errorf("%s container: job warned; log=%q", jobOS, logged)
			}
		})
	}
}
