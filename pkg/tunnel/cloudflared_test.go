package tunnel

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeToken(t *testing.T) {
	payload := `{"a":"acct123","t":"tun-uuid","s":"c2VjcmV0"}`

	for _, tc := range []struct {
		name string
		enc  func(string) string
	}{
		{"std", func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }},
		{"raw std (unpadded)", func(s string) string { return base64.RawStdEncoding.EncodeToString([]byte(s)) }},
		{"url", func(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }},
		{"raw url", func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }},
		{"whitespace tolerated", func(s string) string { return " " + base64.StdEncoding.EncodeToString([]byte(s)) + "\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := decodeToken(tc.enc(payload))
			if err != nil {
				t.Fatalf("decodeToken: %v", err)
			}
			if p.AccountTag != "acct123" || p.TunnelID != "tun-uuid" || p.TunnelSecret != "c2VjcmV0" {
				t.Errorf("unexpected payload: %+v", p)
			}
		})
	}
}

func TestDecodeTokenErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"not base64", "!!!not-base64!!!"},
		{"not json", base64.StdEncoding.EncodeToString([]byte("plaintext"))},
		{"missing fields", base64.StdEncoding.EncodeToString([]byte(`{"a":"only-account"}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeToken(tc.token); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestWriteConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeConfig(path, "/data/creds.json", "tun-uuid", "runner.example.com", 8080); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"tunnel: tun-uuid",
		"credentials-file: /data/creds.json",
		"hostname: runner.example.com",
		"service: http://127.0.0.1:8080",
		"service: http_status:404",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.yml missing %q:\n%s", want, got)
		}
	}
}

func TestNewCloudflaredValidation(t *testing.T) {
	valid := CloudflaredOptions{Token: "tok", Hostname: "h.example.com", DataDir: "/data", Port: 8080}

	if _, err := NewCloudflared(valid); err != nil {
		t.Errorf("valid opts rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*CloudflaredOptions)
	}{
		{"no token", func(o *CloudflaredOptions) { o.Token = "" }},
		{"no hostname", func(o *CloudflaredOptions) { o.Hostname = "" }},
		{"no data dir", func(o *CloudflaredOptions) { o.DataDir = "" }},
		{"no port", func(o *CloudflaredOptions) { o.Port = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := valid
			tc.mutate(&o)
			if _, err := NewCloudflared(o); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}
