package containerd

import (
	"runtime"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// --- SocketPath tests ---

func TestSocketPath_Linux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("testing non-Windows path")
	}
	path := SocketPath("/var/lib/ephemerd")
	if !strings.Contains(path, "containerd.sock") {
		t.Errorf("SocketPath() = %q, expected to contain containerd.sock", path)
	}
	if !strings.HasPrefix(path, "/var/lib/ephemerd") {
		t.Errorf("SocketPath() = %q, expected to start with data dir", path)
	}
}

func TestSocketPath_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("testing Windows named pipe path")
	}
	path := SocketPath(`C:\ProgramData\ephemerd`)
	if !strings.Contains(path, "pipe") {
		t.Errorf("SocketPath() = %q, expected named pipe path on Windows", path)
	}
}

func TestSocketPath_NonEmpty(t *testing.T) {
	path := SocketPath("/data")
	if path == "" {
		t.Error("SocketPath() returned empty string")
	}
}

// --- crlfFormatter tests ---

func TestCRLFFormatter_AddsCarriageReturn(t *testing.T) {
	f := &crlfFormatter{parent: &logrus.TextFormatter{
		DisableTimestamp: true,
		DisableColors:    true,
	}}

	entry := logrus.NewEntry(logrus.StandardLogger())
	entry.Message = "test message"

	b, err := f.Format(entry)
	if err != nil {
		t.Fatalf("Format() error: %v", err)
	}

	s := string(b)
	if !strings.HasSuffix(s, "\r\n") {
		t.Errorf("expected \\r\\n ending, got bytes: %v", b[len(b)-2:])
	}
}

func TestCRLFFormatter_EmptyMessage(t *testing.T) {
	f := &crlfFormatter{parent: &logrus.TextFormatter{
		DisableTimestamp: true,
		DisableColors:    true,
	}}

	entry := logrus.NewEntry(logrus.StandardLogger())
	entry.Message = ""

	b, err := f.Format(entry)
	if err != nil {
		t.Fatalf("Format() error: %v", err)
	}

	if len(b) == 0 {
		t.Fatal("Format() returned empty bytes")
	}
	if !strings.HasSuffix(string(b), "\r\n") {
		t.Errorf("expected \\r\\n ending for empty message")
	}
}

// --- Config type ---

func TestConfig_ZeroValue(t *testing.T) {
	cfg := Config{}
	if cfg.DataDir != "" {
		t.Errorf("zero DataDir = %q", cfg.DataDir)
	}
	if cfg.TCPPort != 0 {
		t.Errorf("zero TCPPort = %d", cfg.TCPPort)
	}
}

// --- crlfFormatter with fields ---

func TestCRLFFormatter_WithFields(t *testing.T) {
	f := &crlfFormatter{parent: &logrus.TextFormatter{
		DisableTimestamp: true,
		DisableColors:    true,
	}}

	entry := logrus.WithFields(logrus.Fields{
		"component": "containerd",
		"port":      10000,
	})
	entry.Message = "server started"

	b, err := f.Format(entry)
	if err != nil {
		t.Fatalf("Format() error: %v", err)
	}

	s := string(b)
	if !strings.HasSuffix(s, "\r\n") {
		t.Errorf("expected \\r\\n ending with fields")
	}
	if !strings.Contains(s, "server started") {
		t.Errorf("message not in output: %q", s)
	}
}

// --- SocketPath consistency ---

func TestSocketPath_DifferentDataDirs(t *testing.T) {
	p1 := SocketPath("/data1")
	p2 := SocketPath("/data2")
	if p1 == p2 && runtime.GOOS != "windows" {
		t.Error("different data dirs should produce different socket paths on non-Windows")
	}
}

func TestSocketPath_WindowsAlwaysSamePipe(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only")
	}
	p1 := SocketPath(`C:\data1`)
	p2 := SocketPath(`C:\data2`)
	if p1 != p2 {
		t.Errorf("Windows named pipe should be constant, got %q vs %q", p1, p2)
	}
}
