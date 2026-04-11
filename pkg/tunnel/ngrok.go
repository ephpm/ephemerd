package tunnel

import (
	"context"
	"fmt"
	"net"
	"os"

	"golang.ngrok.com/ngrok"
	ngrokconfig "golang.ngrok.com/ngrok/config"
)

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
	ln, err := ngrok.Listen(ctx,
		ngrokconfig.HTTPEndpoint(),
		ngrok.WithAuthtoken(n.authtoken),
	)
	if err != nil {
		return nil, fmt.Errorf("ngrok listen: %w", err)
	}
	n.url = ln.URL()
	return ln, nil
}

func (n *Ngrok) PublicURL() string {
	return n.url
}
