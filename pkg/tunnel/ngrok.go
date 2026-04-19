package tunnel

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.ngrok.com/ngrok"
	ngrokconfig "golang.ngrok.com/ngrok/config"
)

const ngrokConnectTimeout = 30 * time.Second

// Ngrok implements Provider using ngrok-go.
type Ngrok struct {
	authtoken string
	url       string
}

// NewNgrok creates an ngrok tunnel provider.
// The auth token is resolved in order: explicit token, NGROK_AUTHTOKEN env var.
func NewNgrok(authtoken string) (*Ngrok, error) {
	if authtoken == "" {
		authtoken = os.Getenv("NGROK_AUTHTOKEN")
	}
	if authtoken == "" {
		return nil, fmt.Errorf("ngrok requires an auth token: set webhook.ngrok_authtoken in config or NGROK_AUTHTOKEN env var")
	}
	return &Ngrok{authtoken: authtoken}, nil
}

func (n *Ngrok) Listen(ctx context.Context) (net.Listener, error) {
	// ngrok-go uses the provided context for the entire session lifetime —
	// cancelling it tears down the tunnel. Pass the caller's context
	// directly so the tunnel lives as long as the caller intends.
	//
	// We still want a connect timeout so startup doesn't hang forever.
	// Use a goroutine + timer: if Listen doesn't return within the
	// timeout, we don't cancel the context (that would be the bug) —
	// we just report the timeout to the caller.
	type result struct {
		ln  net.Listener
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ln, err := ngrok.Listen(ctx,
			ngrokconfig.HTTPEndpoint(),
			ngrok.WithAuthtoken(n.authtoken),
		)
		ch <- result{ln, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("ngrok listen: %w", r.err)
		}
		n.url = "https://" + r.ln.Addr().String()
		return r.ln, nil
	case <-time.After(ngrokConnectTimeout):
		return nil, fmt.Errorf("ngrok listen: connect timeout after %s", ngrokConnectTimeout)
	}
}

func (n *Ngrok) PublicURL() string {
	return n.url
}
