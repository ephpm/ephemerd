package dind

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestHandleContainerAttach_404Unknown verifies the route returns 404 when the
// container id isn't tracked, before any hijack attempt. Catches regressions
// where the handler skips the lookup or panics on missing entries.
func TestHandleContainerAttach_404Unknown(t *testing.T) {
	s := &Server{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		containers: map[string]*containerEntry{},
	}
	req := httptest.NewRequest(http.MethodPost, "/containers/missing/attach", nil)
	w := httptest.NewRecorder()
	s.handleContainerAttach(w, req, "missing")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandleContainerAttach_400WithoutUpgrade verifies a plain POST (no
// Upgrade/Connection headers) is rejected with 400 rather than dropping into
// the hijack path (which would crash since httptest.ResponseRecorder isn't a
// Hijacker). Docker daemon accepts the non-hijack path too, but every CLI
// we care about sends the headers.
func TestHandleContainerAttach_400WithoutUpgrade(t *testing.T) {
	s := &Server{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		containers: map[string]*containerEntry{
			"abc": {ID: "abc", started: make(chan struct{})},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/containers/abc/attach", nil)
	w := httptest.NewRecorder()
	s.handleContainerAttach(w, req, "abc")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestWaitForStart_SignalUnblocks closes the started channel and verifies
// waitForStart returns true before the deadline.
func TestWaitForStart_SignalUnblocks(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	entry := &containerEntry{started: make(chan struct{})}

	go func() {
		time.Sleep(30 * time.Millisecond)
		close(entry.started)
	}()

	if !s.waitForStart(context.Background(), entry, time.Second) {
		t.Error("waitForStart returned false; expected true once started closed")
	}
}

// TestWaitForStart_DeadlineExpires verifies the deadline fires when the
// channel never closes.
func TestWaitForStart_DeadlineExpires(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	entry := &containerEntry{started: make(chan struct{})}

	start := time.Now()
	if s.waitForStart(context.Background(), entry, 50*time.Millisecond) {
		t.Error("waitForStart returned true; expected deadline expiry")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("returned too early: %s", elapsed)
	}
}

// TestWaitForStart_RequestCancelled verifies the request context cancellation
// short-circuits the wait — handles the case where the Docker CLI disconnects
// after sending /attach but before sending /start.
func TestWaitForStart_RequestCancelled(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	entry := &containerEntry{started: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	if s.waitForStart(ctx, entry, time.Second) {
		t.Error("waitForStart returned true; expected false on cancellation")
	}
}

// TestTailLogToWriter_ForwardsThenExitsOnStatus writes content to a log file,
// then signals the task exit, and verifies the streamed content matches and
// the function returns promptly.
func TestTailLogToWriter_ForwardsThenExitsOnStatus(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")

	// Pre-seed the log with some content (handleContainerStart's log file is
	// already opened-for-write by cio.LogFile before we get here).
	if err := os.WriteFile(logPath, []byte("hello world\n"), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	var buf bytes.Buffer
	var mu sync.Mutex
	syncedBuf := &lockedWriter{mu: &mu, w: &buf}

	statusCh := make(chan struct{})
	done := make(chan struct{})

	go func() {
		s.tailLogToWriter(logPath, syncedBuf, statusCh)
		close(done)
	}()

	// Append more content while tailing.
	time.Sleep(30 * time.Millisecond)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("reopen log: %v", err)
	}
	if _, err := f.WriteString("more bytes\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close append: %v", err)
	}

	// Signal task exit and wait for the tailer to drain + return.
	time.Sleep(150 * time.Millisecond)
	close(statusCh)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tailLogToWriter did not return after task exit")
	}

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	want := "hello world\nmore bytes\n"
	if got != want {
		t.Errorf("tailed bytes = %q, want %q", got, want)
	}
}

// TestStdcopyWriter_FrameLayout double-checks the stdcopy header format that
// the attach handler relies on for non-TTY containers (one-byte stream type,
// three NUL padding bytes, four big-endian length bytes, then payload).
//
// stdcopyWriter is defined in exec.go and reused here; keeping the explicit
// regression test next to the attach code makes the frame contract visible
// for anyone tracing how docker run output reaches the CLI.
func TestStdcopyWriter_FrameLayout(t *testing.T) {
	var buf bytes.Buffer
	mu := &sync.Mutex{}
	w := &stdcopyWriter{mu: mu, w: &buf, streamType: 1}
	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := buf.Bytes()
	if len(got) != 10 { // 8-byte header + 2-byte payload
		t.Fatalf("frame size = %d, want 10", len(got))
	}
	if got[0] != 1 {
		t.Errorf("stream type = %d, want 1 (stdout)", got[0])
	}
	for i := 1; i <= 3; i++ {
		if got[i] != 0 {
			t.Errorf("padding byte %d = %d, want 0", i, got[i])
		}
	}
	if size := binary.BigEndian.Uint32(got[4:8]); size != 2 {
		t.Errorf("size field = %d, want 2", size)
	}
	if string(got[8:]) != "hi" {
		t.Errorf("payload = %q, want %q", got[8:], "hi")
	}
}

// lockedWriter is a tiny helper so test goroutines can append to a single
// buffer without data races.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
