package tunnel

import (
	"context"
	"fmt"
	"net"
)

// Provider creates a publicly-reachable listener for receiving webhooks.
// Implementations manage the tunnel lifecycle: Listen creates the tunnel,
// and closing the returned listener (or cancelling the context) tears it down.
type Provider interface {
	// Listen creates a tunnel and returns a net.Listener with a public URL.
	// The tunnel is torn down when the listener is closed or ctx is cancelled.
	Listen(ctx context.Context) (net.Listener, error)

	// PublicURL returns the public URL of the tunnel after Listen succeeds.
	PublicURL() string
}

// New creates a tunnel provider from config.
func New(provider, authtoken, baseURL string) (Provider, error) {
	switch provider {
	case "ngrok":
		return NewNgrok(authtoken)
	case "localtunnel":
		return NewLocalTunnel(baseURL), nil
	default:
		return nil, fmt.Errorf("unknown tunnel provider: %q (supported: ngrok, localtunnel)", provider)
	}
}
