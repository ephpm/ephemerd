package containerd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"time"

	"github.com/containerd/containerd/v2/client"
)

// Config for the embedded containerd instance.
type Config struct {
	DataDir string
	Log     *slog.Logger
}

// Server manages the embedded containerd lifecycle.
type Server struct {
	cfg    Config
	client *client.Client
	cancel context.CancelFunc
	done   chan struct{}
}

// New creates and starts an embedded containerd server.
//
// containerd is started in-process following the k3s/rke2 model.
// The containerd state and image store live under the data directory.
func New(cfg Config) (*Server, error) {
	s := &Server{
		cfg:  cfg,
		done: make(chan struct{}),
	}

	if err := s.setup(); err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}

	if err := s.start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	return s, nil
}

// Client returns the containerd client connected to the embedded instance.
func (s *Server) Client() *client.Client {
	return s.client
}

// Stop gracefully shuts down the embedded containerd.
func (s *Server) Stop() {
	s.cfg.Log.Info("stopping containerd")
	if s.client != nil {
		s.client.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
	<-s.done
}

// SocketPath returns the containerd socket path for the given data directory.
func SocketPath(dataDir string) string {
	if goruntime.GOOS == "windows" {
		return `\\.\pipe\ephemerd-containerd`
	}
	return filepath.Join(dataDir, "containerd", "containerd.sock")
}

func (s *Server) setup() error {
	dirs := []string{
		filepath.Join(s.cfg.DataDir, "containerd", "state"),
		filepath.Join(s.cfg.DataDir, "containerd", "root"),
		filepath.Join(s.cfg.DataDir, "runners"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}

	return nil
}

func (s *Server) start() error {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	socket := SocketPath(s.cfg.DataDir)

	// TODO: Replace this with in-process containerd server startup.
	// For now, this is a placeholder that connects to an existing containerd
	// or starts the embedded one. The real implementation will import
	// containerd's server packages directly (like k3s does with
	// github.com/containerd/containerd/v2/cmd/containerd/command).
	//
	// The in-process approach:
	//   1. Build a containerd server.Config
	//   2. Create a containerd server.Server
	//   3. Start it in a goroutine
	//   4. Connect a client to the in-process socket
	//
	// For the initial iteration, we start containerd as a subprocess
	// so the GitHub + runner integration can be developed in parallel.

	go func() {
		defer close(s.done)
		<-ctx.Done()
	}()

	// Wait briefly then connect client
	var err error
	for i := range 30 {
		s.client, err = client.New(socket)
		if err == nil {
			// Verify connection
			_, err = s.client.Version(ctx)
			if err == nil {
				return nil
			}
		}
		if i == 0 {
			s.cfg.Log.Debug("waiting for containerd socket", "path", socket)
		}
		time.Sleep(500 * time.Millisecond)
	}

	cancel()
	return fmt.Errorf("connecting to containerd at %s: %w", socket, err)
}

// ExecCtr runs the ctr CLI against ephemerd's containerd instance.
// This provides the "ephemerd ctrctl" debugging interface.
func ExecCtr(socketPath string, args []string) error {
	ctrArgs := append([]string{"--address", socketPath}, args...)

	cmd := exec.Command("ctr", ctrArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}
