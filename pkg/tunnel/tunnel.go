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

// Options are the union of settings any provider might need. Fields
// unused by the chosen provider are ignored — this keeps the constructor
// signature stable as new providers land.
type Options struct {
	Provider string // "ngrok" | "localtunnel" | "cloudflared"

	// ngrok
	NgrokAuthtoken string

	// localtunnel
	LocalTunnelBaseURL string

	// cloudflared
	CloudflaredToken    string
	CloudflaredHostname string
	CloudflaredVersion  string
	CloudflaredDataDir  string // <ephemerd data dir> — provider carves out cloudflared/ under this
	CloudflaredPort     int    // local webhook port cloudflared forwards to
}

// New creates a tunnel provider from the options.
func New(o Options) (Provider, error) {
	switch o.Provider {
	case "ngrok":
		return NewNgrok(o.NgrokAuthtoken)
	case "localtunnel":
		return NewLocalTunnel(o.LocalTunnelBaseURL), nil
	case "cloudflared":
		return NewCloudflared(CloudflaredOptions{
			Token:    o.CloudflaredToken,
			Hostname: o.CloudflaredHostname,
			Version:  o.CloudflaredVersion,
			DataDir:  o.CloudflaredDataDir,
			Port:     o.CloudflaredPort,
		})
	default:
		return nil, fmt.Errorf("unknown tunnel provider: %q (supported: ngrok, localtunnel, cloudflared)", o.Provider)
	}
}
