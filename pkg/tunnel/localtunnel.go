package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/ephpm/ephemerd/pkg/localtunnel"
)

const (
	// attemptTimeout is how long each tunnel connection attempt gets.
	attemptTimeout = 10 * time.Second
	// listenTimeout is the total time to keep retrying before giving up.
	listenTimeout = 2 * time.Minute
)

// LocalTunnel implements Provider using localtunnel.
type LocalTunnel struct {
	baseURL string
	url     string
}

// NewLocalTunnel creates a localtunnel provider.
// baseURL is optional — if empty, uses the public localtunnel service (loca.lt).
// Set baseURL to use a self-hosted localtunnel server.
func NewLocalTunnel(baseURL string) *LocalTunnel {
	return &LocalTunnel{baseURL: baseURL}
}

// Listen establishes a tunnel, retrying on failure. Each attempt gets a 10s
// timeout. Retries continue for up to 2 minutes or until ctx is cancelled.
// This is blocking — if localtunnel is configured, ephemerd cannot receive
// webhooks without a tunnel.
func (lt *LocalTunnel) Listen(ctx context.Context) (net.Listener, error) {
	opts := localtunnel.Options{}
	if lt.baseURL != "" {
		opts.BaseURL = lt.baseURL
	}

	deadline := time.After(listenTimeout)
	var lastErr error
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		ln, err := localtunnel.Listen(attemptCtx, opts)
		cancel()

		if err == nil {
			lt.url = ln.URL()
			if attempt > 1 {
				slog.Info("localtunnel connected after retries", "attempt", attempt, "url", lt.url)
			}
			return ln, nil
		}

		lastErr = err
		slog.Warn("localtunnel attempt failed, retrying", "attempt", attempt, "error", err)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("localtunnel listen cancelled: %w", ctx.Err())
		case <-deadline:
			return nil, fmt.Errorf("localtunnel listen failed after %s (%d attempts): %w", listenTimeout, attempt, lastErr)
		case <-time.After(time.Second):
			// brief pause before retry
		}
	}
}

func (lt *LocalTunnel) PublicURL() string {
	return lt.url
}
