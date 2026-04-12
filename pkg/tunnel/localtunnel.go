package tunnel

import (
	"context"
	"fmt"
	"net"

	"github.com/ephpm/ephemerd/pkg/localtunnel"
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

func (lt *LocalTunnel) Listen(ctx context.Context) (net.Listener, error) {
	opts := localtunnel.Options{}
	if lt.baseURL != "" {
		opts.BaseURL = lt.baseURL
	}

	ln, err := localtunnel.Listen(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("localtunnel listen: %w", err)
	}
	lt.url = ln.URL()

	return ln, nil
}

func (lt *LocalTunnel) PublicURL() string {
	return lt.url
}
