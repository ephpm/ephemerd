package dind

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
)

// TestStdcopyWriterFraming pins the Docker stdcopy framing format so that
// `docker exec -i` clients (notably buildx's docker-container driver) can
// demultiplex stdout (1) and stderr (2) over the hijacked stream.
func TestStdcopyWriterFraming(t *testing.T) {
	var buf bytes.Buffer
	mu := &sync.Mutex{}
	stdout := &stdcopyWriter{mu: mu, w: &buf, streamType: 1}
	stderr := &stdcopyWriter{mu: mu, w: &buf, streamType: 2}

	if _, err := stdout.Write([]byte("hello")); err != nil {
		t.Fatalf("stdout write: %v", err)
	}
	if _, err := stderr.Write([]byte("world!")); err != nil {
		t.Fatalf("stderr write: %v", err)
	}

	got := buf.Bytes()
	// Frame 1: stdout "hello" (5 bytes)
	if got[0] != 1 {
		t.Errorf("frame 1 type = %d, want 1 (stdout)", got[0])
	}
	if size := binary.BigEndian.Uint32(got[4:8]); size != 5 {
		t.Errorf("frame 1 size = %d, want 5", size)
	}
	if !bytes.Equal(got[8:13], []byte("hello")) {
		t.Errorf("frame 1 payload = %q, want %q", got[8:13], "hello")
	}
	// Frame 2: stderr "world!" (6 bytes), starting at offset 13.
	if got[13] != 2 {
		t.Errorf("frame 2 type = %d, want 2 (stderr)", got[13])
	}
	if size := binary.BigEndian.Uint32(got[17:21]); size != 6 {
		t.Errorf("frame 2 size = %d, want 6", size)
	}
	if !bytes.Equal(got[21:27], []byte("world!")) {
		t.Errorf("frame 2 payload = %q, want %q", got[21:27], "world!")
	}
}

// TestStdcopyWriterReservedBytes verifies bytes 1-3 of the header are zero.
// Some Docker clients sanity-check this — moby/moby/pkg/stdcopy enforces it
// when demuxing.
func TestStdcopyWriterReservedBytes(t *testing.T) {
	var buf bytes.Buffer
	mu := &sync.Mutex{}
	w := &stdcopyWriter{mu: mu, w: &buf, streamType: 1}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	hdr := buf.Bytes()[:8]
	if hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0 {
		t.Errorf("reserved bytes hdr[1..4] = %v, want 0,0,0", hdr[1:4])
	}
}

// TestStdcopyWriterConcurrent ensures two writers sharing a mutex never
// interleave a header with a different stream's payload.
func TestStdcopyWriterConcurrent(t *testing.T) {
	var buf bytes.Buffer
	mu := &sync.Mutex{}
	stdout := &stdcopyWriter{mu: mu, w: &buf, streamType: 1}
	stderr := &stdcopyWriter{mu: mu, w: &buf, streamType: 2}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := stdout.Write([]byte("aaaa")); err != nil {
				t.Errorf("stdout: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := stderr.Write([]byte("bbbb")); err != nil {
				t.Errorf("stderr: %v", err)
			}
		}()
	}
	wg.Wait()

	// Walk the buffer and verify each frame is well-formed and payload matches stream type.
	got := buf.Bytes()
	for offset := 0; offset < len(got); {
		if offset+8 > len(got) {
			t.Fatalf("truncated header at offset %d", offset)
		}
		streamType := got[offset]
		size := int(binary.BigEndian.Uint32(got[offset+4 : offset+8]))
		if offset+8+size > len(got) {
			t.Fatalf("truncated payload at offset %d (size=%d)", offset, size)
		}
		payload := got[offset+8 : offset+8+size]
		switch streamType {
		case 1:
			if !bytes.Equal(payload, []byte("aaaa")) {
				t.Fatalf("stdout frame at offset %d has wrong payload: %q", offset, payload)
			}
		case 2:
			if !bytes.Equal(payload, []byte("bbbb")) {
				t.Fatalf("stderr frame at offset %d has wrong payload: %q", offset, payload)
			}
		default:
			t.Fatalf("unknown stream type %d at offset %d", streamType, offset)
		}
		offset += 8 + size
	}
}
