//go:build e2e && windows

// Package e2e — BuildKit end-to-end test on Windows.
//
// Starts an embedded containerd, constructs pkg/buildkit.Server against it,
// and uses the buildkit client to exercise the in-process control plane:
// worker listing + disk usage. This verifies that the Controller, worker,
// Dockerfile frontend, and bufconn plumbing all initialize correctly on a
// real Windows host.
//
// Does NOT currently exercise a full Solve (pulling a Windows base image
// takes several minutes on first run). A Solve test is the natural
// follow-up once we've pre-seeded a test image into the containerd image
// store.
//
// Run with:
//
//	go test -tags 'e2e windows' -v -timeout 5m -run TestE2E_Buildkit ./test/e2e/
package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/ephpm/ephemerd/pkg/buildkit"
	containerdpkg "github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/networking"
	bkclient "github.com/moby/buildkit/client"
	fsutil "github.com/tonistiigi/fsutil"
)

// TestE2E_Buildkit_StartAndListWorkers starts ephemerd's embedded
// containerd and BuildKit, then asks the Controller to list workers via
// the in-process buildkit client. Passing this means:
//
//   - pkg/buildkit.NewServer successfully constructed session manager,
//     worker controller, cache store, history DB, and Controller.
//   - The containerd worker dialed ephemerd's containerd named pipe and
//     registered itself.
//   - The Controller's bufconn gRPC listener is serving and the
//     client.Client can reach it.
//
// Does not exercise a Solve — that requires a base image in the store
// (see the follow-up TestE2E_Buildkit_Solve_* once we seed one).
func TestE2E_Buildkit_StartAndListWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping buildkit e2e in short mode")
	}

	dataDir, err := os.MkdirTemp("", "ephemerd-bk-e2e-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Logf("cleanup: remove %s: %v", dataDir, err)
		}
	})

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Start embedded containerd.
	ctrd, err := containerdpkg.New(containerdpkg.Config{
		DataDir: dataDir,
		Log:     log.With("component", "containerd"),
	})
	if err != nil {
		t.Fatalf("containerd start: %v", err)
	}
	t.Cleanup(func() {
		ctrd.Stop()
	})
	t.Logf("containerd ready at %s", containerdpkg.SocketPath(dataDir))

	// Construct BuildKit server against it.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bk, err := buildkit.NewServer(ctx, buildkit.Config{
		DataDir:             filepath.Join(dataDir, "buildkit"),
		ContainerdAddress:   containerdpkg.SocketPath(dataDir),
		ContainerdNamespace: "buildkit",
		Log:                 log.With("component", "buildkit"),
	})
	if err != nil {
		t.Fatalf("buildkit NewServer: %v", err)
	}
	t.Cleanup(func() {
		if err := bk.Close(); err != nil {
			t.Logf("cleanup: buildkit close: %v", err)
		}
	})
	t.Log("buildkit ready")

	// Ask the in-process Controller for its worker list.
	client, err := bk.Client(ctx)
	if err != nil {
		t.Fatalf("buildkit client dial: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Logf("cleanup: buildkit client close: %v", err)
		}
	})

	workers, err := client.ListWorkers(ctx)
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if len(workers) == 0 {
		t.Fatal("expected at least one worker, got 0")
	}
	for _, w := range workers {
		t.Logf("worker id=%s labels=%v platforms=%v", w.ID, w.Labels, w.Platforms)
	}

	// Sanity check: we configured a single containerd-backed worker, so
	// we should see exactly one with our ephemerd label.
	if len(workers) != 1 {
		t.Errorf("want 1 worker, got %d", len(workers))
	}
	if got := workers[0].Labels["org.ephpm.ephemerd"]; got != "true" {
		t.Errorf("worker missing ephemerd label: got Labels=%v", workers[0].Labels)
	}
}

// TestE2E_Buildkit_Solve_Servercore performs a full docker build against
// the embedded BuildKit solver on Windows:
//
//  1. Start embedded containerd
//  2. Import C:\ProgramData\ephemerd\images\servercore-ltsc2025.tar into
//     the "buildkit" containerd namespace
//  3. Start buildkit.Server
//  4. Solve a minimal Dockerfile that only adds a LABEL — no RUN — so we
//     exercise the image pipeline without paying for a Hyper-V container
//     boot per step.
//  5. Verify the built image appears in the containerd "buildkit" namespace.
//
// This is the real "can ephemerd build Windows images" test.
func TestE2E_Buildkit_Solve_Servercore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping buildkit solve e2e in short mode")
	}
	const baseImage = "mcr.microsoft.com/windows/servercore:ltsc2025"
	const baseTarball = `C:\ProgramData\ephemerd\images\servercore-ltsc2025.tar`
	if _, err := os.Stat(baseTarball); err != nil {
		t.Skipf("base tarball not staged at %s: %v", baseTarball, err)
	}

	dataDir, err := os.MkdirTemp("", "ephemerd-bk-solve-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Logf("cleanup: remove %s: %v", dataDir, err)
		}
	})

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// 1. Start embedded containerd.
	ctrd, err := containerdpkg.New(containerdpkg.Config{
		DataDir: dataDir,
		Log:     log.With("component", "containerd"),
	})
	if err != nil {
		t.Fatalf("containerd start: %v", err)
	}
	t.Cleanup(ctrd.Stop)
	t.Logf("containerd ready at %s", containerdpkg.SocketPath(dataDir))

	// 2. Import the servercore tarball into the buildkit namespace.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	bkNamespace := "buildkit"
	ctrdClient := ctrd.Client()
	nsCtx := namespaces.WithNamespace(ctx, bkNamespace)

	t.Logf("importing %s into namespace %q (may take 30-60s for a ~2 GB tarball)",
		baseTarball, bkNamespace)
	importStart := time.Now()
	f, err := os.Open(baseTarball)
	if err != nil {
		t.Fatalf("open base tarball: %v", err)
	}
	imgs, importErr := ctrdClient.Import(nsCtx, f, client.WithAllPlatforms(true))
	if closeErr := f.Close(); closeErr != nil {
		t.Logf("close base tarball: %v", closeErr)
	}
	if importErr != nil {
		t.Fatalf("importing base tarball: %v", importErr)
	}
	t.Logf("imported %d images in %s", len(imgs), time.Since(importStart))
	for _, img := range imgs {
		t.Logf("  - %s", img.Name)
	}

	// 3. Start BuildKit against the running containerd.
	bk, err := buildkit.NewServer(ctx, buildkit.Config{
		DataDir:             filepath.Join(dataDir, "buildkit"),
		ContainerdAddress:   containerdpkg.SocketPath(dataDir),
		ContainerdNamespace: bkNamespace,
		Log:                 log.With("component", "buildkit"),
	})
	if err != nil {
		t.Fatalf("buildkit NewServer: %v", err)
	}
	t.Cleanup(func() {
		if err := bk.Close(); err != nil {
			t.Logf("cleanup: buildkit close: %v", err)
		}
	})
	t.Log("buildkit ready")

	// 4. Write a minimal Dockerfile to a temp context dir.
	ctxDir := t.TempDir()
	dockerfile := fmt.Sprintf(
		"FROM %s\nLABEL org.ephpm.test=yes\n",
		baseImage,
	)
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	// Mount the context via fsutil.FS (LocalMounts is the non-deprecated API).
	ctxFS, err := fsutil.NewFS(ctxDir)
	if err != nil {
		t.Fatalf("fsutil.NewFS: %v", err)
	}

	const outTag = "ephemerd-test/servercore-labeled:e2e"
	solveOpt := bkclient.SolveOpt{
		Frontend: "dockerfile.v0",
		FrontendAttrs: map[string]string{
			"filename": "Dockerfile",
			// Default image-resolve-mode already checks the local
			// containerd image store before pulling. We pre-imported
			// servercore above, so the local path should hit.
		},
		LocalMounts: map[string]fsutil.FS{
			"context":    ctxFS,
			"dockerfile": ctxFS,
		},
		Exports: []bkclient.ExportEntry{{
			Type: "image",
			Attrs: map[string]string{
				"name": outTag,
			},
		}},
	}

	// 5. Drive the solve. statusCh streams progress.
	statusCh := make(chan *bkclient.SolveStatus, 32)
	go func() {
		for s := range statusCh {
			for _, v := range s.Vertexes {
				msg := "running"
				if v.Completed != nil {
					msg = "done"
					if v.Error != "" {
						msg = "FAILED: " + v.Error
					}
				} else if v.Started != nil {
					msg = "started"
				}
				t.Logf("  [solve] %s — %s", v.Name, msg)
			}
		}
	}()

	t.Log("starting solve")
	solveStart := time.Now()
	resp, err := bk.Build(ctx, solveOpt, statusCh)
	if err != nil {
		t.Fatalf("solve failed after %s: %v", time.Since(solveStart), err)
	}
	t.Logf("solve completed in %s", time.Since(solveStart))
	for k, v := range resp.ExporterResponse {
		t.Logf("  export: %s=%s", k, v)
	}

	// 6. Verify the built image is present in the buildkit namespace.
	imgList, err := ctrdClient.ListImages(nsCtx)
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	var found bool
	for _, img := range imgList {
		t.Logf("  image: %s", img.Name())
		if img.Name() == outTag {
			found = true
		}
	}
	if !found {
		t.Errorf("expected image %q in containerd store, not found", outTag)
	}
}

// TestE2E_Buildkit_NetworkProbe diagnoses why RUN steps in BuildKit-built
// Windows images can't resolve DNS even when the HCN endpoint is attached.
// The runner container path (pkg/runtime) gives Hyper-V isolated containers
// working DNS via the same HCN setup, but BuildKit's containerdexecutor
// produces containers where Invoke-WebRequest fails with "remote name could
// not be resolved". This test runs probe commands inside a build container
// to surface exactly which layer is broken: adapter, DNS server config,
// route, or NAT.
//
// Probes are run with $ErrorActionPreference = 'Continue' so all of them
// execute regardless of individual failures; the BuildKit log captures
// stdout from each.
func TestE2E_Buildkit_NetworkProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network probe e2e in short mode")
	}
	const baseImage = "mcr.microsoft.com/windows/servercore:ltsc2025"
	const baseTarball = `C:\ProgramData\ephemerd\images\servercore-ltsc2025.tar`
	if _, err := os.Stat(baseTarball); err != nil {
		t.Skipf("base tarball not staged at %s: %v", baseTarball, err)
	}

	dataDir, err := os.MkdirTemp("", "ephemerd-bk-netprobe-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Logf("cleanup: remove %s: %v", dataDir, err)
		}
	})

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctrd, err := containerdpkg.New(containerdpkg.Config{
		DataDir: dataDir,
		Log:     log.With("component", "containerd"),
	})
	if err != nil {
		t.Fatalf("containerd start: %v", err)
	}
	t.Cleanup(ctrd.Stop)

	// Real networking — same HCN NAT network the runner uses.
	netMgr, err := networking.New(networking.Config{
		DataDir: dataDir,
		Log:     log.With("component", "networking"),
	})
	if err != nil {
		t.Fatalf("networking.New: %v", err)
	}
	t.Cleanup(netMgr.Cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	bkNamespace := "buildkit"
	ctrdClient := ctrd.Client()
	nsCtx := namespaces.WithNamespace(ctx, bkNamespace)

	t.Logf("importing %s", baseTarball)
	f, err := os.Open(baseTarball)
	if err != nil {
		t.Fatalf("open base tarball: %v", err)
	}
	imgs, importErr := ctrdClient.Import(nsCtx, f, client.WithAllPlatforms(true))
	if closeErr := f.Close(); closeErr != nil {
		t.Logf("close: %v", closeErr)
	}
	if importErr != nil {
		t.Fatalf("import: %v", importErr)
	}
	for _, img := range imgs {
		t.Logf("imported: %s", img.Name)
	}

	bk, err := buildkit.NewServer(ctx, buildkit.Config{
		DataDir:             filepath.Join(dataDir, "buildkit"),
		ContainerdAddress:   containerdpkg.SocketPath(dataDir),
		ContainerdNamespace: bkNamespace,
		Network:             netMgr,
		Log:                 log.With("component", "buildkit"),
	})
	if err != nil {
		t.Fatalf("buildkit NewServer: %v", err)
	}
	t.Cleanup(func() {
		if err := bk.Close(); err != nil {
			t.Logf("cleanup: buildkit close: %v", err)
		}
	})

	// Diagnostic Dockerfile — every probe runs unconditionally so we see
	// the full picture even when one probe fails.
	ctxDir := t.TempDir()
	// Single RUN that runs all probes serially. Parallel RUN ops in
	// BuildKit cause LLB to fan them out; for the diagnostic we want one
	// container, not seven, so the output is sequential and a hang is
	// localized.
	dockerfile := fmt.Sprintf(`FROM %s
SHELL ["powershell", "-Command", "$ErrorActionPreference = 'Continue';"]
RUN Write-Host '=== ipconfig /all ==='; ipconfig /all; `+
		`Write-Host '=== Get-DnsClientServerAddress ==='; Get-DnsClientServerAddress | Format-Table; `+
		`Write-Host '=== route print ==='; route print; `+
		`Write-Host '=== Resolve-DnsName 8.8.8.8 ==='; Resolve-DnsName 8.8.8.8 -ErrorAction SilentlyContinue; `+
		`Write-Host '=== Resolve-DnsName go.dev ==='; Resolve-DnsName go.dev -ErrorAction SilentlyContinue; `+
		`Write-Host '=== Test-NetConnection 8.8.8.8:53 ==='; Test-NetConnection 8.8.8.8 -Port 53 -InformationLevel Detailed; `+
		`Write-Host '=== Test-NetConnection 8.8.8.8:443 ==='; Test-NetConnection 8.8.8.8 -Port 443 -InformationLevel Detailed
`, baseImage)

	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	ctxFS, err := fsutil.NewFS(ctxDir)
	if err != nil {
		t.Fatalf("fsutil.NewFS: %v", err)
	}

	const outTag = "ephemerd-test/netprobe:e2e"
	solveOpt := bkclient.SolveOpt{
		Frontend: "dockerfile.v0",
		FrontendAttrs: map[string]string{
			"filename": "Dockerfile",
		},
		LocalMounts: map[string]fsutil.FS{
			"context":    ctxFS,
			"dockerfile": ctxFS,
		},
		Exports: []bkclient.ExportEntry{{
			Type: "image",
			Attrs: map[string]string{
				"name": outTag,
			},
		}},
	}

	statusCh := make(chan *bkclient.SolveStatus, 64)
	go func() {
		for s := range statusCh {
			for _, v := range s.Vertexes {
				msg := "running"
				if v.Completed != nil {
					msg = "done"
					if v.Error != "" {
						msg = "FAILED: " + v.Error
					}
				} else if v.Started != nil {
					msg = "started"
				}
				t.Logf("[vtx] %s — %s", v.Name, msg)
			}
			for _, l := range s.Logs {
				t.Logf("[log] %s: %s", l.Vertex.String()[:8], string(l.Data))
			}
		}
	}()

	// If the solve hangs, dump every goroutine stack so we can see where
	// the executor is wedged (network provider New, container create,
	// task wait, etc).
	dumpCh := time.AfterFunc(90*time.Second, func() {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Logf("=== GOROUTINE DUMP (solve hung past 90s) ===\n%s", buf[:n])
	})

	t.Log("starting probe solve")
	start := time.Now()
	_, err = bk.Build(ctx, solveOpt, statusCh)
	dumpCh.Stop()
	t.Logf("solve returned after %s err=%v", time.Since(start), err)
	if err != nil {
		t.Logf("solve error (expected if any RUN step fails): %v", err)
	}
}

// dockerTarRef extracts the first RepoTag from a Docker-style OCI tarball's
// manifest.json. Tests that need to know what's inside a tarball before
// importing can use this as a sanity check.
func dockerTarRef(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("manifest.json not found in %s", path)
		}
		if err != nil {
			return "", err
		}
		if hdr.Name != "manifest.json" {
			continue
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			return "", err
		}
		// Minimal parse: look for "RepoTags":["...
		data := buf.String()
		const marker = `"RepoTags":["`
		i := bytes.Index([]byte(data), []byte(marker))
		if i < 0 {
			return "", nil
		}
		rest := data[i+len(marker):]
		j := bytes.IndexByte([]byte(rest), '"')
		if j < 0 {
			return "", nil
		}
		return rest[:j], nil
	}
}
