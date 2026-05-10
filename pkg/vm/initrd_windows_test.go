//go:build windows

package vm

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCPIOEntry_HeaderFormat(t *testing.T) {
	var buf bytes.Buffer
	data := []byte("hello")
	if err := writeCPIOEntry(&buf, "foo", 0o100644, data, ""); err != nil {
		t.Fatalf("writeCPIOEntry: %v", err)
	}
	out := buf.Bytes()

	// newc magic
	if !bytes.HasPrefix(out, []byte("070701")) {
		t.Errorf("missing newc magic 070701; got first 6 bytes %q", out[:6])
	}

	// Header is 110 bytes (6 magic + 13*8 fields), then null-terminated name
	if len(out) < 110+4 {
		t.Fatalf("output too short: %d bytes", len(out))
	}

	// Name "foo" + NUL = 4 bytes. Header+name = 114; pad to 116 (next 4-aligned).
	wantName := append([]byte("foo"), 0)
	if !bytes.Equal(out[110:110+len(wantName)], wantName) {
		t.Errorf("name bytes mismatch; got %q want %q", out[110:114], wantName)
	}

	// After header+name+pad, the body "hello" follows.
	hdrAndName := 110 + 4
	bodyStart := hdrAndName + ((4 - hdrAndName%4) % 4)
	if !bytes.Equal(out[bodyStart:bodyStart+5], data) {
		t.Errorf("body mismatch; got %q want %q", out[bodyStart:bodyStart+5], data)
	}
}

func TestWriteCPIOEntry_Symlink(t *testing.T) {
	var buf bytes.Buffer
	if err := writeCPIOEntry(&buf, "ln", 0o120777, nil, "target"); err != nil {
		t.Fatalf("writeCPIOEntry: %v", err)
	}
	// Body should be the link target, not data.
	if !bytes.Contains(buf.Bytes(), []byte("target")) {
		t.Error("symlink body should contain link target")
	}
}

func TestWriteCPIOEntry_FourByteAlignment(t *testing.T) {
	// Pick a name whose length forces non-zero padding.
	var buf bytes.Buffer
	name := "ab" // header(110) + name+nul(3) = 113 → pad 3 bytes to reach 116
	if err := writeCPIOEntry(&buf, name, 0o100644, []byte("x"), ""); err != nil {
		t.Fatalf("writeCPIOEntry: %v", err)
	}
	// Total length must be 4-aligned: header+name+pad + data+pad.
	if buf.Len()%4 != 0 {
		t.Errorf("cpio entry not 4-byte aligned: %d bytes", buf.Len())
	}
}

func TestBuildBootInitrd_AppendsEphemerdLinux(t *testing.T) {
	dir := t.TempDir()

	// Fake base initrd: a single-entry gzipped cpio with a sentinel file.
	basePath := filepath.Join(dir, "initrd-base")
	if err := writeGzippedCPIO(basePath, map[string][]byte{
		"sentinel": []byte("base-content"),
	}); err != nil {
		t.Fatalf("writing base initrd: %v", err)
	}

	// Fake ephemerd-linux binary.
	binPath := filepath.Join(dir, "ephemerd-linux")
	binContent := []byte("fake elf data 12345")
	if err := os.WriteFile(binPath, binContent, 0o755); err != nil {
		t.Fatalf("writing binary: %v", err)
	}

	destPath := filepath.Join(dir, "initrd")
	if err := buildBootInitrd(basePath, binPath, destPath); err != nil {
		t.Fatalf("buildBootInitrd: %v", err)
	}

	// Read back the boot initrd. It should be concatenated gzip streams:
	// the base + our appended assets/ephemerd-linux entry.
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("reading boot initrd: %v", err)
	}
	baseData, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("reading base: %v", err)
	}
	if !bytes.HasPrefix(got, baseData) {
		t.Errorf("boot initrd does not start with base content")
	}
	if len(got) <= len(baseData) {
		t.Errorf("boot initrd not larger than base: got=%d base=%d", len(got), len(baseData))
	}

	// The appended part is a separate gzip stream after the base.
	tail := got[len(baseData):]
	gr, err := gzip.NewReader(bytes.NewReader(tail))
	if err != nil {
		t.Fatalf("appended tail is not gzip: %v", err)
	}
	defer func() { _ = gr.Close() }()
	cpio, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("reading appended cpio: %v", err)
	}
	// Cpio should contain the ephemerd-linux body.
	if !bytes.Contains(cpio, binContent) {
		t.Error("appended cpio does not contain ephemerd-linux body")
	}
	if !bytes.Contains(cpio, []byte("assets/ephemerd-linux")) {
		t.Error("appended cpio does not contain assets/ephemerd-linux path")
	}
	if !bytes.Contains(cpio, []byte("TRAILER!!!")) {
		t.Error("appended cpio missing TRAILER!!! terminator")
	}
}

func TestBuildBootInitrd_MissingBase(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "ephemerd-linux")
	if err := os.WriteFile(binPath, []byte("data"), 0o755); err != nil {
		t.Fatalf("writing binary: %v", err)
	}
	err := buildBootInitrd(filepath.Join(dir, "missing-base"), binPath, filepath.Join(dir, "out"))
	if err == nil {
		t.Error("expected error for missing base initrd")
	}
}

func TestBuildBootInitrd_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "initrd-base")
	if err := writeGzippedCPIO(basePath, map[string][]byte{"x": []byte("y")}); err != nil {
		t.Fatalf("writing base: %v", err)
	}
	err := buildBootInitrd(basePath, filepath.Join(dir, "missing-bin"), filepath.Join(dir, "out"))
	if err == nil {
		t.Error("expected error for missing ephemerd-linux")
	}
}

// writeGzippedCPIO is a test helper that emits a tiny valid gzipped newc cpio
// archive containing the given files.
func writeGzippedCPIO(path string, files map[string][]byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gw := gzip.NewWriter(f)
	for name, data := range files {
		if err := writeCPIOEntry(gw, name, 0o100644, data, ""); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	if err := writeCPIOEntry(gw, "TRAILER!!!", 0, nil, ""); err != nil {
		return err
	}
	return gw.Close()
}
