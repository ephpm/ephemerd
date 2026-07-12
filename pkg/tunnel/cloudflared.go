package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CloudflaredOptions configures the Cloudflared provider.
type CloudflaredOptions struct {
	Token    string // cloudflared tunnel token (base64-encoded JSON with account_tag, tunnel_id, secret)
	Hostname string // public FQDN of the tunnel (for PublicURL)
	Version  string // cloudflared version to download if binary missing; if empty, uses defaultCloudflaredVersion
	DataDir  string // ephemerd data dir; provider carves out <DataDir>/cloudflared/ for binary + config
	Port     int    // local port cloudflared forwards to (matches ephemerd's webhook listener)
}

const (
	defaultCloudflaredVersion = "2026.6.1"
	subprocessShutdownWait    = 15 * time.Second
	readyTimeout              = 45 * time.Second
	readyPollInterval         = 500 * time.Millisecond
)

// Cloudflared runs cloudflared as a managed subprocess bound to ephemerd's
// lifetime. On Linux the child gets SIGTERM the moment the parent thread
// exits (Pdeathsig) — cloudflared cannot outlive ephemerd, even under
// SIGKILL or panic. On other platforms the guarantee is best-effort via
// Close().
type Cloudflared struct {
	opts CloudflaredOptions
	dir  string

	mu       sync.Mutex
	cmd      *exec.Cmd
	ln       net.Listener
	waitDone chan struct{} // closed by the single waiter goroutine after cmd.Wait returns
	closing  bool          // set by close() so the waiter can tell shutdown from crash
}

// NewCloudflared validates options and returns a provider. It does not
// download the binary or start the subprocess — that happens in Listen.
func NewCloudflared(opts CloudflaredOptions) (*Cloudflared, error) {
	if opts.Token == "" {
		return nil, errors.New("cloudflared: token is required")
	}
	if opts.Hostname == "" {
		return nil, errors.New("cloudflared: hostname is required")
	}
	if opts.DataDir == "" {
		return nil, errors.New("cloudflared: data_dir is required")
	}
	if opts.Port == 0 {
		return nil, errors.New("cloudflared: port is required")
	}
	if opts.Version == "" {
		opts.Version = defaultCloudflaredVersion
	}
	return &Cloudflared{
		opts: opts,
		dir:  filepath.Join(opts.DataDir, "cloudflared"),
	}, nil
}

// PublicURL returns the https URL cloudflared exposes to the internet.
func (c *Cloudflared) PublicURL() string {
	return "https://" + c.opts.Hostname
}

// Listen starts the local webhook listener, ensures the cloudflared binary
// is present, writes config + credentials, and spawns cloudflared bound to
// this process's lifetime. The returned listener is what ephemerd Accepts
// on; closing it also stops cloudflared.
func (c *Cloudflared) Listen(ctx context.Context) (net.Listener, error) {
	wrapped, metricsAddr, err := c.start(ctx)
	if err != nil {
		return nil, err
	}

	// Wait for at least one edge connection before returning, so webhook
	// registration (which fires immediately after Listen) doesn't race the
	// connect. ngrok/localtunnel block in Listen the same way. A timeout is
	// a warning, not an error — the tunnel usually catches up moments later
	// and GitHub keeps the webhook registered even if its ping 530s.
	// Runs outside the mutex so the crash-watcher goroutine is never blocked.
	if err := waitForReady(ctx, metricsAddr); err != nil {
		slog.Warn("cloudflared did not report ready in time; continuing anyway", "error", err)
	}

	return wrapped, nil
}

// start does the mutex-guarded portion of Listen: filesystem prep, local
// listener bind, and subprocess launch.
func (c *Cloudflared) start(ctx context.Context) (net.Listener, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cmd != nil {
		return nil, "", errors.New("cloudflared: already listening")
	}

	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("cloudflared: mkdir data dir: %w", err)
	}

	binary, err := ensureCloudflaredBinary(ctx, c.dir, c.opts.Version)
	if err != nil {
		return nil, "", err
	}

	creds, err := decodeToken(c.opts.Token)
	if err != nil {
		return nil, "", err
	}

	credsPath := filepath.Join(c.dir, "creds.json")
	if err := writeCredentials(credsPath, creds); err != nil {
		return nil, "", err
	}
	configPath := filepath.Join(c.dir, "config.yml")
	if err := writeConfig(configPath, credsPath, creds.TunnelID, c.opts.Hostname, c.opts.Port); err != nil {
		return nil, "", err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", c.opts.Port))
	if err != nil {
		return nil, "", fmt.Errorf("cloudflared: listen on 127.0.0.1:%d: %w", c.opts.Port, err)
	}

	// Reserve a port for cloudflared's metrics endpoint so we can poll
	// /ready. Bind-then-close is racy in theory; in practice the window is
	// microseconds and cloudflared re-binding it immediately makes collisions
	// vanishingly unlikely on a single-purpose VM.
	metricsAddr, err := reserveLoopbackAddr()
	if err != nil {
		_ = ln.Close()
		return nil, "", fmt.Errorf("cloudflared: reserve metrics port: %w", err)
	}

	cmd := exec.CommandContext(ctx, binary,
		"tunnel", "--config", configPath, "--no-autoupdate",
		"--metrics", metricsAddr,
		"run",
	)
	cmd.Stdout = &slogWriter{level: slog.LevelInfo, prefix: "cloudflared"}
	cmd.Stderr = &slogWriter{level: slog.LevelWarn, prefix: "cloudflared"}
	// On ctx cancellation, ask nicely first; Go force-kills after WaitDelay.
	// Without these, CommandContext's default Cancel is an immediate SIGKILL.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = subprocessShutdownWait
	applyPdeathsig(cmd) // Linux-only; no-op elsewhere

	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		return nil, "", fmt.Errorf("cloudflared: start subprocess: %w", err)
	}
	c.cmd = cmd
	c.ln = ln
	c.closing = false
	c.waitDone = make(chan struct{})

	slog.Info("cloudflared started",
		"pid", cmd.Process.Pid,
		"hostname", c.opts.Hostname,
		"local_port", c.opts.Port,
	)

	// Single waiter: the only goroutine allowed to call cmd.Wait. On
	// unexpected exit it closes the listener so ephemerd's Accept loop
	// unwinds and the scheduler treats it as a fault. close() synchronizes
	// on waitDone instead of calling Wait itself.
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		closing := c.closing
		ln := c.ln
		c.mu.Unlock()
		if !closing {
			if err != nil {
				slog.Warn("cloudflared exited", "error", err)
			} else {
				slog.Warn("cloudflared exited unexpectedly with code 0")
			}
			if ln != nil {
				_ = ln.Close()
			}
		}
		close(c.waitDone)
	}()

	return &cfListener{Listener: ln, parent: c}, metricsAddr, nil
}

// close is the internal teardown — closes the listener, SIGTERMs cloudflared,
// waits up to subprocessShutdownWait for the waiter goroutine to observe the
// exit, then falls back to KILL.
func (c *Cloudflared) close() error {
	c.mu.Lock()
	cmd := c.cmd
	ln := c.ln
	waitDone := c.waitDone
	c.closing = true
	c.cmd = nil
	c.ln = nil
	c.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		slog.Debug("signaling cloudflared SIGTERM", "error", err)
	}

	select {
	case <-waitDone:
		return nil
	case <-time.After(subprocessShutdownWait):
		slog.Warn("cloudflared did not exit within shutdown timeout; sending KILL")
		_ = cmd.Process.Kill()
		<-waitDone
		return nil
	}
}

// cfListener wraps the raw local listener so ln.Close() also stops cloudflared.
type cfListener struct {
	net.Listener
	parent *Cloudflared
	once   sync.Once
}

func (l *cfListener) Close() error {
	var err error
	l.once.Do(func() { err = l.parent.close() })
	return err
}

// reserveLoopbackAddr binds an ephemeral loopback port and releases it,
// returning the address for cloudflared's --metrics flag.
func reserveLoopbackAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	return addr, l.Close()
}

// waitForReady polls cloudflared's /ready metrics endpoint until it reports
// an established edge connection (HTTP 200), the timeout lapses, or ctx ends.
func waitForReady(ctx context.Context, metricsAddr string) error {
	deadline := time.After(readyTimeout)
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()

	url := "http://" + metricsAddr + "/ready"
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("not ready after %s", readyTimeout)
		case <-ticker.C:
		}
	}
}

// tokenPayload is the JSON structure encoded in a cloudflared tunnel token.
// Cloudflare uses short field names on the wire.
type tokenPayload struct {
	AccountTag   string `json:"a"`
	TunnelID     string `json:"t"`
	TunnelSecret string `json:"s"`
}

// decodeToken parses a cloudflared tunnel token: base64-encoded JSON.
// Dashboard-issued tokens are frequently unpadded, so try all four base64
// variants before giving up.
func decodeToken(token string) (*tokenPayload, error) {
	token = strings.TrimSpace(token)
	var dec []byte
	var err error
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		dec, err = enc.DecodeString(token)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("cloudflared: decode token: %w", err)
	}
	var p tokenPayload
	if err := json.Unmarshal(dec, &p); err != nil {
		return nil, fmt.Errorf("cloudflared: parse token: %w", err)
	}
	if p.AccountTag == "" || p.TunnelID == "" || p.TunnelSecret == "" {
		return nil, errors.New("cloudflared: token is missing account/tunnel/secret fields")
	}
	return &p, nil
}

func writeCredentials(path string, p *tokenPayload) error {
	// This is the format cloudflared's credentials-file expects — see
	// https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/create-local-tunnel/
	body := map[string]string{
		"AccountTag":   p.AccountTag,
		"TunnelID":     p.TunnelID,
		"TunnelName":   "ephemerd-managed",
		"TunnelSecret": p.TunnelSecret,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func writeConfig(path, credsPath, tunnelID, hostname string, port int) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "tunnel: %s\n", tunnelID)
	fmt.Fprintf(&sb, "credentials-file: %s\n", credsPath)
	fmt.Fprintln(&sb, "protocol: http2")
	fmt.Fprintln(&sb, "ingress:")
	fmt.Fprintf(&sb, "  - hostname: %s\n", hostname)
	fmt.Fprintf(&sb, "    service: http://127.0.0.1:%d\n", port)
	fmt.Fprintln(&sb, "  - service: http_status:404")
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

// slogWriter forwards cloudflared subprocess output to slog line-by-line.
// Each instance is written to by a single pipe-copy goroutine, so no locking.
type slogWriter struct {
	level  slog.Level
	prefix string
	buf    []byte
}

func (w *slogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if line == "" {
			continue
		}
		slog.Log(context.Background(), w.level, w.prefix, "line", line)
	}
	return len(p), nil
}
