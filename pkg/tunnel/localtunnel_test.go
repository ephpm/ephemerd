package tunnel

import (
	"testing"
)

func TestNewLocalTunnel_EmptyBaseURL(t *testing.T) {
	lt := NewLocalTunnel("")
	if lt == nil {
		t.Fatal("NewLocalTunnel returned nil")
	}
	if lt.baseURL != "" {
		t.Errorf("baseURL = %q, want empty string", lt.baseURL)
	}
}

func TestNewLocalTunnel_CustomBaseURL(t *testing.T) {
	lt := NewLocalTunnel("https://my-localtunnel.example.com")
	if lt == nil {
		t.Fatal("NewLocalTunnel returned nil")
	}
	if lt.baseURL != "https://my-localtunnel.example.com" {
		t.Errorf("baseURL = %q, want %q", lt.baseURL, "https://my-localtunnel.example.com")
	}
}

func TestPublicURL_BeforeListen(t *testing.T) {
	lt := NewLocalTunnel("")
	if url := lt.PublicURL(); url != "" {
		t.Errorf("PublicURL() before Listen = %q, want empty string", url)
	}
}

func TestPublicURL_BeforeListen_WithBaseURL(t *testing.T) {
	lt := NewLocalTunnel("https://tunnels.example.com")
	if url := lt.PublicURL(); url != "" {
		t.Errorf("PublicURL() before Listen = %q, want empty string", url)
	}
}

func TestNewLocalTunnel_StructFields(t *testing.T) {
	lt := NewLocalTunnel("https://custom.tunnel.io")

	// Verify all fields after construction
	if lt.baseURL != "https://custom.tunnel.io" {
		t.Errorf("baseURL = %q, want %q", lt.baseURL, "https://custom.tunnel.io")
	}
	if lt.url != "" {
		t.Errorf("url = %q, want empty (not connected yet)", lt.url)
	}
}

func TestNewLocalTunnel_DefaultBaseURL(t *testing.T) {
	// When baseURL is empty, the LocalTunnel struct stores empty.
	// The actual default (loca.lt) is applied in Listen() via the
	// localtunnel.Options struct.
	lt := NewLocalTunnel("")
	if lt.baseURL != "" {
		t.Errorf("empty constructor should store empty baseURL, got %q", lt.baseURL)
	}
	if lt.url != "" {
		t.Errorf("url should be empty before Listen, got %q", lt.url)
	}
}
