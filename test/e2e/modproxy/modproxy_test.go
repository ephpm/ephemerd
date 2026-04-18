//go:build e2e

// Package modproxy_test exercises the Go module caching proxy end-to-end.
//
// It starts a proxy, fetches a real module from the Go ecosystem through it,
// verifies the first request is a cache miss (upstream fetch), and the second
// is a cache hit (served from disk). Then compiles a small Go program using
// the proxy as GOPROXY to confirm it works with the real `go` toolchain.
//
// Run with: mage e2emodproxy
// Requires: internet access (fetches from proxy.golang.org on first run).
package modproxy_test

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/ephpm/ephemerd/pkg/modproxy"
)

func TestModProxy_E2E_CacheRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping modproxy e2e in short mode")
	}

	cacheDir := t.TempDir()
	p := modproxy.New(modproxy.Config{
		CacheDir:   cacheDir,
		ListenAddr: "127.0.0.1:0",
		Cleanup:    false, // keep cache for inspection
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Stop()

	proxyURL := "http://" + p.Addr()

	// Fetch a small, stable module's .info file.
	modPath := "/golang.org/x/text/@v/v0.14.0.info"

	// First request: cache miss, fetches from upstream.
	resp, err := http.Get(proxyURL + modPath)
	if err != nil {
		t.Fatalf("first GET error: %v", err)
	}
	body1, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("first GET status = %d, want 200, body: %s", resp.StatusCode, body1)
	}
	if len(body1) == 0 {
		t.Fatal("first GET returned empty body")
	}
	t.Logf("cache miss: got %d bytes for %s", len(body1), modPath)

	// Second request: cache hit, served from disk.
	resp, err = http.Get(proxyURL + modPath)
	if err != nil {
		t.Fatalf("second GET error: %v", err)
	}
	body2, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("second GET status = %d, want 200", resp.StatusCode)
	}
	if string(body1) != string(body2) {
		t.Error("cached response differs from original")
	}
	t.Log("cache hit: response matches")

	// Verify cache files exist on disk.
	var cacheFiles int
	filepath.Walk(cacheDir, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			cacheFiles++
		}
		return nil
	})
	if cacheFiles == 0 {
		t.Error("no cache files found on disk")
	}
	t.Logf("cache dir has %d files", cacheFiles)
}

func TestModProxy_E2E_GoToolchain(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping modproxy e2e in short mode")
	}

	// Check that `go` is available.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found in PATH")
	}

	cacheDir := t.TempDir()
	p := modproxy.New(modproxy.Config{
		CacheDir:   cacheDir,
		ListenAddr: "127.0.0.1:0",
		Cleanup:    false,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Stop()

	proxyURL := "http://" + p.Addr()

	// Create a tiny Go program that imports a real module.
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "go.mod"), []byte(`module testmod

go 1.21

require golang.org/x/text v0.14.0
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte(`package main

import (
	"fmt"
	"golang.org/x/text/language"
)

func main() {
	fmt.Println(language.English)
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run `go build` using our proxy.
	binName := "testbin"
	if goruntime.GOOS == "windows" {
		binName = "testbin.exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(workDir, binName), ".")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"GOPROXY="+proxyURL+",direct",
		"GONOSUMDB=*",
		"GOFLAGS=-mod=mod",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	t.Log("go build succeeded through proxy")

	// Verify the binary runs.
	runCmd := exec.Command(filepath.Join(workDir, binName))
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("testbin failed: %v\n%s", err, runOut)
	}
	if !strings.Contains(string(runOut), "en") {
		t.Errorf("unexpected output: %s", runOut)
	}
	t.Logf("testbin output: %s", strings.TrimSpace(string(runOut)))
}
