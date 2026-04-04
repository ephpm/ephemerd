package containerd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"time"

	"github.com/containerd/containerd/v2/client"
	srvconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
	ctdserver "github.com/containerd/containerd/v2/cmd/containerd/server"
)

// Config for the embedded containerd instance.
type Config struct {
	DataDir string
	Log     *slog.Logger
}

// Server manages the in-process containerd lifecycle.
type Server struct {
	cfg    Config
	srv    *ctdserver.Server
	client *client.Client
	cancel context.CancelFunc
	done   chan struct{}
}

// New creates and starts an embedded containerd server in-process.
//
// containerd runs as a Go library in a goroutine, following the k3s/rke2
// model. No external containerd binary is needed.
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

// Stop gracefully shuts down the embedded containerd server.
func (s *Server) Stop() {
	s.cfg.Log.Info("stopping containerd")

	if s.client != nil {
		s.client.Close()
	}

	if s.srv != nil {
		s.srv.Stop()
	}

	if s.cancel != nil {
		s.cancel()
	}

	<-s.done
	s.cfg.Log.Info("containerd stopped")
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
	rootDir := filepath.Join(s.cfg.DataDir, "containerd", "root")
	stateDir := filepath.Join(s.cfg.DataDir, "containerd", "state")

	// Remove stale socket from a previous run
	if goruntime.GOOS != "windows" {
		os.Remove(socket)
	}

	// Build containerd server config
	cfg := &srvconfig.Config{
		Version: 2,
		Root:    rootDir,
		State:   stateDir,
	}
	cfg.GRPC.Address = socket

	// Create the in-process containerd server
	srv, err := ctdserver.New(ctx, cfg)
	if err != nil {
		cancel()
		return fmt.Errorf("creating containerd server: %w", err)
	}
	s.srv = srv

	// Create gRPC listener and serve in background
	l, err := net.Listen("unix", socket)
	if err != nil {
		srv.Stop()
		cancel()
		return fmt.Errorf("listening on %s: %w", socket, err)
	}

	go func() {
		defer close(s.done)
		if err := srv.ServeGRPC(l); err != nil {
			select {
			case <-ctx.Done():
			default:
				s.cfg.Log.Error("containerd gRPC server error", "error", err)
			}
		}
	}()

	// Also serve tTRPC for container task/event APIs
	ttrpcSocket := socket + ".ttrpc"
	if goruntime.GOOS != "windows" {
		os.Remove(ttrpcSocket)
	}
	tl, err := net.Listen("unix", ttrpcSocket)
	if err != nil {
		s.cfg.Log.Warn("failed to start tTRPC listener, some features may not work", "error", err)
	} else {
		go func() {
			if err := srv.ServeTTRPC(tl); err != nil {
				select {
				case <-ctx.Done():
				default:
					s.cfg.Log.Error("containerd tTRPC server error", "error", err)
				}
			}
		}()
	}

	s.cfg.Log.Info("containerd server started in-process", "socket", socket)

	// Connect client to the in-process server
	for i := range 30 {
		s.client, err = client.New(socket)
		if err == nil {
			_, err = s.client.Version(ctx)
			if err == nil {
				s.cfg.Log.Info("containerd ready")
				return nil
			}
		}
		if i == 0 {
			s.cfg.Log.Debug("waiting for containerd to be ready", "socket", socket)
		}
		time.Sleep(500 * time.Millisecond)
	}

	srv.Stop()
	cancel()
	return fmt.Errorf("timed out connecting to containerd at %s: %w", socket, err)
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
