package tunnel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"runtime"
	"strings"
	"testing"
)

func TestCloudflaredAsset(t *testing.T) {
	url, isTgz := cloudflaredAsset("2026.6.1")
	if !strings.HasPrefix(url, "https://github.com/cloudflare/cloudflared/releases/download/2026.6.1/cloudflared-") {
		t.Fatalf("unexpected url: %s", url)
	}
	switch runtime.GOOS {
	case "darwin":
		if !isTgz || !strings.HasSuffix(url, ".tgz") {
			t.Errorf("darwin should be a .tgz archive, got url=%s isTgz=%v", url, isTgz)
		}
	case "windows":
		if isTgz || !strings.HasSuffix(url, ".exe") {
			t.Errorf("windows should be a bare .exe, got url=%s isTgz=%v", url, isTgz)
		}
	default:
		if isTgz || strings.HasSuffix(url, ".tgz") {
			t.Errorf("%s should be a bare binary, got url=%s isTgz=%v", runtime.GOOS, url, isTgz)
		}
	}
}

func TestExtractCloudflaredTgz(t *testing.T) {
	// Build a tgz whose single entry is a `cloudflared` binary.
	want := []byte("#!/bin/sh\necho fake-cloudflared\n")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: "cloudflared", Mode: 0o755, Size: int64(len(want)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(want); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gw.Close()

	var out bytes.Buffer
	if err := extractCloudflaredTgz(&buf, &out); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("extracted %q, want %q", out.Bytes(), want)
	}
}

func TestExtractCloudflaredTgzMissingEntry(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "README", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	_ = gw.Close()

	if err := extractCloudflaredTgz(&buf, &bytes.Buffer{}); err == nil {
		t.Error("expected error when archive lacks a cloudflared entry")
	}
}
