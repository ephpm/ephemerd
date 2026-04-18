package scheduler

import (
	"crypto/ed25519"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"

	"golang.org/x/crypto/ssh"
)

// VMSSHInfo contains the information needed to SSH into a macOS VM.
type VMSSHInfo struct {
	IP         string `json:"ip"`
	User       string `json:"user"`
	PrivateKey []byte `json:"private_key"` // PEM-encoded ed25519
}

// registerVMSSHHandler adds a /vm/ssh-info endpoint to the given mux.
// Only accessible via the unix control socket (root-owned, 0600).
func (s *Scheduler) registerVMSSHHandler(mux *http.ServeMux) {
	mux.HandleFunc("/vm/ssh-info", func(w http.ResponseWriter, r *http.Request) {
		jobIDStr := r.URL.Query().Get("job_id")
		if jobIDStr == "" {
			http.Error(w, "job_id required", http.StatusBadRequest)
			return
		}
		var jobID int64
		if _, err := fmt.Sscanf(jobIDStr, "%d", &jobID); err != nil {
			http.Error(w, "invalid job_id", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		rj, exists := s.running[jobID]
		s.mu.Unlock()
		if !exists {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}

		if rj.macosVM == nil {
			http.Error(w, "job is not a macOS VM job", http.StatusBadRequest)
			return
		}

		ip := rj.macosVM.RunnerAddress()
		if ip == "" {
			http.Error(w, "VM IP not yet discovered", http.StatusServiceUnavailable)
			return
		}

		// Get the ephemeral SSH private key from the macOS VM config
		s.mu.Lock()
		signer := s.cfg.MacOSVMConfig.SSHSigner
		s.mu.Unlock()

		var keyPEM []byte
		if key, ok := signer.(ed25519.PrivateKey); ok {
			pemBlock, err := ssh.MarshalPrivateKey(key, "")
			if err == nil {
				keyPEM = pem.EncodeToMemory(pemBlock)
			}
		}

		info := VMSSHInfo{
			IP:         ip,
			User:       "admin",
			PrivateKey: keyPEM,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(info); err != nil {
			s.cfg.Log.Warn("failed to encode SSH info response", "error", err)
		}
	})
}

// StartVMSSHServer starts a small HTTP server on the unix control socket
// for the VM SSH info endpoint. Called after the gRPC server is set up.
func (s *Scheduler) StartVMSSHServer() (func(), error) {
	socketPath := SocketPath(s.cfg.DataDir) + ".http"

	_ = os.Remove(socketPath)

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("chmod %s: %w", socketPath, err)
	}

	mux := http.NewServeMux()
	s.registerVMSSHHandler(mux)

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(lis) }()

	return func() {
		_ = srv.Close()
		_ = os.Remove(socketPath)
	}, nil
}
