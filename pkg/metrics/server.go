package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServerConfig configures the metrics HTTP server.
type ServerConfig struct {
	Port    int
	Path    string
	TLSCert string
	TLSKey  string
	// BindAddr is the interface the metrics listener binds to. Empty defaults
	// to "127.0.0.1" so the endpoint (which has no authentication) is not
	// exposed on all interfaces by default. Operators who scrape from another
	// host set this explicitly (e.g. "0.0.0.0") and are expected to firewall
	// the port and/or front it with TLS.
	BindAddr string
	Log      *slog.Logger
}

// Serve starts the metrics HTTP server and blocks until ctx is cancelled.
// Returns a cleanup function that shuts down the server gracefully.
func Serve(ctx context.Context, cfg ServerConfig) func() {
	mux := http.NewServeMux()
	mux.Handle(cfg.Path, promhttp.Handler())

	// Default to loopback: the metrics endpoint is unauthenticated, so binding
	// all interfaces would leak host/runner telemetry to anything on the
	// network. Operators opt into a wider bind via BindAddr.
	bindAddr := cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", bindAddr, cfg.Port),
		Handler: mux,
	}

	go func() {
		var err error
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			cfg.Log.Info("metrics server listening (TLS)", "bind", bindAddr, "port", cfg.Port, "path", cfg.Path)
			err = server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			cfg.Log.Info("metrics server listening", "bind", bindAddr, "port", cfg.Port, "path", cfg.Path)
			err = server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			cfg.Log.Error("metrics server error", "error", err)
		}
	}()

	return func() {
		if err := server.Shutdown(context.Background()); err != nil {
			cfg.Log.Error("metrics server shutdown error", "error", err)
		}
	}
}
