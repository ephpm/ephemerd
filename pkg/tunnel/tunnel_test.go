package tunnel

import (
	"testing"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		authtoken string
		baseURL   string
		wantType  string
		wantErr   bool
	}{
		{
			name:      "ngrok with token",
			provider:  "ngrok",
			authtoken: "fake-token",
			wantType:  "*tunnel.Ngrok",
		},
		{
			name:     "ngrok without token errors",
			provider: "ngrok",
			wantErr:  true,
		},
		{
			name:     "localtunnel default",
			provider: "localtunnel",
			wantType: "*tunnel.LocalTunnel",
		},
		{
			name:     "localtunnel with base URL",
			provider: "localtunnel",
			baseURL:  "https://tunnels.example.com",
			wantType: "*tunnel.LocalTunnel",
		},
		{
			name:     "unknown provider",
			provider: "cloudflare",
			wantErr:  true,
		},
		{
			name:     "cloudflared without token errors",
			provider: "cloudflared",
			wantErr:  true,
		},
		{
			name:     "empty provider",
			provider: "",
			wantErr:  true,
		},
	}

	// Ensure NGROK_AUTHTOKEN doesn't interfere with tests
	t.Setenv("NGROK_AUTHTOKEN", "")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(Options{
				Provider:           tt.provider,
				NgrokAuthtoken:     tt.authtoken,
				LocalTunnelBaseURL: tt.baseURL,
			})
			if tt.wantErr {
				if err == nil {
					t.Errorf("New(%q) = nil error, want error", tt.provider)
				}
				return
			}
			if err != nil {
				t.Errorf("New(%q) error = %v", tt.provider, err)
				return
			}
			if p == nil {
				t.Errorf("New(%q) returned nil provider", tt.provider)
			}
		})
	}
}

func TestNgrokAuthTokenFromEnv(t *testing.T) {
	t.Setenv("NGROK_AUTHTOKEN", "env-token")

	n, err := NewNgrok("")
	if err != nil {
		t.Fatalf("NewNgrok with env token: %v", err)
	}
	if n.authtoken != "env-token" {
		t.Errorf("authtoken = %q, want %q", n.authtoken, "env-token")
	}
}

func TestNgrokExplicitTokenOverridesEnv(t *testing.T) {
	t.Setenv("NGROK_AUTHTOKEN", "env-token")

	n, err := NewNgrok("explicit-token")
	if err != nil {
		t.Fatalf("NewNgrok with explicit token: %v", err)
	}
	if n.authtoken != "explicit-token" {
		t.Errorf("authtoken = %q, want %q", n.authtoken, "explicit-token")
	}
}

func TestNgrokNoTokenErrors(t *testing.T) {
	t.Setenv("NGROK_AUTHTOKEN", "")

	_, err := NewNgrok("")
	if err == nil {
		t.Error("NewNgrok with no token should error")
	}
}

func TestNgrokPublicURLBeforeListen(t *testing.T) {
	n := &Ngrok{authtoken: "fake"}
	if url := n.PublicURL(); url != "" {
		t.Errorf("PublicURL before Listen = %q, want empty", url)
	}
}

func TestLocalTunnelNew(t *testing.T) {
	lt := NewLocalTunnel("")
	if lt.baseURL != "" {
		t.Errorf("baseURL = %q, want empty", lt.baseURL)
	}

	lt = NewLocalTunnel("https://tunnels.example.com")
	if lt.baseURL != "https://tunnels.example.com" {
		t.Errorf("baseURL = %q, want %q", lt.baseURL, "https://tunnels.example.com")
	}
}

func TestLocalTunnelPublicURLBeforeListen(t *testing.T) {
	lt := NewLocalTunnel("")
	if url := lt.PublicURL(); url != "" {
		t.Errorf("PublicURL before Listen = %q, want empty", url)
	}
}

func TestProviderInterface(t *testing.T) {
	// Compile-time check that both types implement Provider
	var _ Provider = (*Ngrok)(nil)
	var _ Provider = (*LocalTunnel)(nil)
}
