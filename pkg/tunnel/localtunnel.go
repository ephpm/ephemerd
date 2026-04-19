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
		// localtunnel.Listen stores the context in the Listener and uses
		// it for the entire session lifetime (proxy goroutines, Accept).
		// We must NOT cancel it after Listen returns — that would kill
		// the tunnel immediately. Use a goroutine + timer for the
		// connect timeout instead.
		type result struct {
			ln  *localtunnel.Listener
			err error
		}
		ch := make(chan result, 1)
		go func() {
			ln, err := localtunnel.Listen(ctx, opts)
			ch <- result{ln, err}
		}()

		var ln *localtunnel.Listener
		var err error
		select {
		case r := <-ch:
			ln, err = r.ln, r.err
		case <-time.After(attemptTimeout):
			err = fmt.Errorf("connect timeout after %s", attemptTimeout)
		}

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
