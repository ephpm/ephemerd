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
	Log     *slog.Logger
}

// Serve starts the metrics HTTP server and blocks until ctx is cancelled.
// Returns a cleanup function that shuts down the server gracefully.
func Serve(ctx context.Context, cfg ServerConfig) func() {
	mux := http.NewServeMux()
	mux.Handle(cfg.Path, promhttp.Handler())

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: mux,
	}

	go func() {
		var err error
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			cfg.Log.Info("metrics server listening (TLS)", "port", cfg.Port, "path", cfg.Path)
			err = server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			cfg.Log.Info("metrics server listening", "port", cfg.Port, "path", cfg.Path)
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
