package dind

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// TestStreamToTempFile_Small writes a small payload and verifies it round-trips.
func TestStreamToTempFile_Small(t *testing.T) {
	body := []byte("hello world")
	tmpPath, err := streamToTempFile(bytes.NewReader(body), "dind-test-*.tar")
	if err != nil {
		t.Fatalf("streamToTempFile: %v", err)
	}
	t.Cleanup(func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			t.Logf("remove: %v", rmErr)
		}
	})

	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(got), len(body))
	}

	// File handle must be closed — re-opening should succeed.
	f, err := os.OpenFile(tmpPath, os.O_RDWR, 0)
	if err != nil {
		t.Errorf("reopen: %v", err)
	} else if cerr := f.Close(); cerr != nil {
		t.Logf("close: %v", cerr)
	}
}

// TestStreamToTempFile_LargeFile writes >1MB to verify io.Copy works at scale.
func TestStreamToTempFile_LargeFile(t *testing.T) {
	const size = 2 * 1024 * 1024 // 2 MiB
	// Use a pattern that writes a deterministic payload so we can verify
	// every byte rather than just the length.
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i % 251)
	}

	tmpPath, err := streamToTempFile(bytes.NewReader(body), "dind-test-large-*.tar")
	if err != nil {
		t.Fatalf("streamToTempFile: %v", err)
	}
	t.Cleanup(func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			t.Logf("remove: %v", rmErr)
		}
	})

	info, err := os.Stat(tmpPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != int64(size) {
		t.Fatalf("size = %d, want %d", info.Size(), size)
	}

	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != size {
		t.Fatalf("len(got) = %d, want %d", len(got), size)
	}
	// Spot-check a handful of bytes rather than full bytes.Equal — for 2MiB
	// the comparison is fast either way, but spot-checks make failures
	// easier to interpret.
	for _, idx := range []int{0, 1, 1024, 65535, 1_000_000, size - 1} {
		if got[idx] != byte(idx%251) {
			t.Errorf("byte %d = %d, want %d", idx, got[idx], idx%251)
		}
	}
}

// chunkedReader returns bytes from chunks one at a time, simulating the
// kind of staggered reads net/http delivers from a slow client.
type chunkedReader struct {
	chunks [][]byte
	idx    int
	off    int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.idx >= len(c.chunks) {
		return 0, io.EOF
	}
	chunk := c.chunks[c.idx]
	if c.off >= len(chunk) {
		c.idx++
		c.off = 0
		return c.Read(p)
	}
	n := copy(p, chunk[c.off:])
	c.off += n
	return n, nil
}

// TestStreamToTempFile_PartialWrites simulates a reader that returns the
// payload in small pieces, the way an HTTP body would.
func TestStreamToTempFile_PartialWrites(t *testing.T) {
	chunks := [][]byte{
		[]byte("first "),
		[]byte("second "),
		[]byte("third "),
		[]byte("fourth"),
	}
	r := &chunkedReader{chunks: chunks}

	tmpPath, err := streamToTempFile(r, "dind-partial-*.tar")
	if err != nil {
		t.Fatalf("streamToTempFile: %v", err)
	}
	t.Cleanup(func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			t.Logf("remove: %v", rmErr)
		}
	})

	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	want := "first second third fourth"
	if string(got) != want {
		t.Errorf("body = %q, want %q", string(got), want)
	}
}

// errAfterReader returns n bytes then an error, modeling a connection that
// dies mid-stream.
type errAfterReader struct {
	prefix []byte
	off    int
	err    error
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if e.off >= len(e.prefix) {
		return 0, e.err
	}
	n := copy(p, e.prefix[e.off:])
	e.off += n
	return n, nil
}

// TestStreamToTempFile_ReadError ensures a mid-stream error is surfaced and
// the partial temp file is cleaned up.
func TestStreamToTempFile_ReadError(t *testing.T) {
	boom := errors.New("connection reset")
	r := &errAfterReader{prefix: []byte("partial body"), err: boom}

	tmpPath, err := streamToTempFile(r, "dind-err-*.tar")
	if err == nil {
		// Stream succeeded somehow — clean up to avoid clutter.
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			t.Logf("remove: %v", rmErr)
		}
		t.Fatal("expected error from streamToTempFile, got nil")
	}
	if !strings.Contains(err.Error(), "writing") {
		t.Errorf("err = %v, want 'writing temp file' wrapper", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("err = %v, should surface underlying error", err)
	}
	// Temp file should not exist (we cleaned it up).
	if tmpPath != "" {
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			t.Errorf("temp file %s should have been removed on error", tmpPath)
			if rmErr := os.Remove(tmpPath); rmErr != nil {
				t.Logf("cleanup remove: %v", rmErr)
			}
		}
	}
}

// TestStreamToTempFile_Empty ensures a zero-byte body produces a zero-byte file.
func TestStreamToTempFile_Empty(t *testing.T) {
	tmpPath, err := streamToTempFile(bytes.NewReader(nil), "dind-empty-*.tar")
	if err != nil {
		t.Fatalf("streamToTempFile: %v", err)
	}
	t.Cleanup(func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			t.Logf("remove: %v", rmErr)
		}
	})

	info, err := os.Stat(tmpPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("size = %d, want 0", info.Size())
	}
}

// TestStreamToTempFile_PatternUsed checks that the temp file name matches
// the supplied pattern (tar suffix preserved).
func TestStreamToTempFile_PatternUsed(t *testing.T) {
	tmpPath, err := streamToTempFile(bytes.NewReader([]byte("x")), "ephemerd-test-*.tar")
	if err != nil {
		t.Fatalf("streamToTempFile: %v", err)
	}
	t.Cleanup(func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			t.Logf("remove: %v", rmErr)
		}
	})
	if !strings.HasSuffix(tmpPath, ".tar") {
		t.Errorf("tmpPath = %q, want .tar suffix", tmpPath)
	}
	if !strings.Contains(tmpPath, "ephemerd-test-") {
		t.Errorf("tmpPath = %q, want pattern prefix", tmpPath)
	}
}
