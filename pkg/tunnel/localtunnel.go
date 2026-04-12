package tunnel

import (
	"context"
	"fmt"
	"net"

	localtunnel "github.com/localtunnel/go-localtunnel"
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

	// localtunnel.Listen does not accept a context and can hang indefinitely
	// (e.g. waiting for TCP connections from an unreliable free server).
	// Run it in a goroutine and race against the context.
	type result struct {
		ln  *localtunnel.Listener
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ln, err := localtunnel.Listen(opts)
		ch <- result{ln, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("localtunnel listen: %w", r.err)
		}
		lt.url = r.ln.URL()

		// Close listener when context is cancelled
		go func() {
			<-ctx.Done()
			_ = r.ln.Close()
		}()

		return r.ln, nil
	}
}

func (lt *LocalTunnel) PublicURL() string {
	return lt.url
}
