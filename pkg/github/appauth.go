package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// AppAuth manages GitHub App installation tokens with automatic refresh.
// Installation tokens expire after 1 hour; this refreshes at 45 minutes.
type AppAuth struct {
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	log            *slog.Logger

	// tokenURL overrides the GitHub API token endpoint for tests. When
	// empty, the production https://api.github.com/... URL is used.
	tokenURL string
	// httpClient overrides the http client used for the token exchange.
	// When nil, http.DefaultClient is used.
	httpClient *http.Client

	mu      sync.RWMutex
	token   string
	expires time.Time

	// refreshMu serializes on-demand token refreshes so concurrent callers
	// don't thunder into GitHub's token endpoint.
	refreshMu sync.Mutex

	done chan struct{}
}

// NewAppAuth loads the PEM key and returns an AppAuth that mints and
// refreshes GitHub App installation tokens.
func NewAppAuth(appID, installationID int64, keyPath string, log *slog.Logger) (*AppAuth, error) {
	// Expand ~ in path
	if strings.HasPrefix(keyPath, "~") {
		home, _ := os.UserHomeDir()
		keyPath = home + keyPath[1:]
	}

	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading app private key %s: %w", keyPath, err)
	}

	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from %s", keyPath)
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}

	a := &AppAuth{
		appID:          appID,
		installationID: installationID,
		key:            key,
		log:            log,
		done:           make(chan struct{}),
	}

	// Get initial token
	if err := a.refresh(); err != nil {
		return nil, fmt.Errorf("initial token exchange: %w", err)
	}

	// Start background refresh loop
	go a.refreshLoop()

	return a, nil
}

// Token returns the current valid installation token, refreshing it
// synchronously if the cached token expires within 5 minutes. The background
// refreshLoop is still the primary path; this is a safety net so a missed
// background refresh doesn't cause repeated 401s for hours.
func (a *AppAuth) Token() string {
	a.mu.RLock()
	tok := a.token
	remaining := time.Until(a.expires)
	a.mu.RUnlock()

	if remaining > 5*time.Minute {
		return tok
	}
	// Token is expired or expiring soon — refresh synchronously, serialized
	// via refreshMu so concurrent Token() callers collapse into a single
	// GitHub round-trip. Re-check expiry once we hold the lock in case
	// another goroutine already refreshed.
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	a.mu.RLock()
	remaining = time.Until(a.expires)
	a.mu.RUnlock()
	if remaining > 5*time.Minute {
		a.mu.RLock()
		defer a.mu.RUnlock()
		return a.token
	}

	if err := a.refresh(); err != nil {
		a.log.Warn("on-demand token refresh failed", "error", err)
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.token
}

// Stop halts the background refresh goroutine.
func (a *AppAuth) Stop() {
	close(a.done)
}

func (a *AppAuth) refreshLoop() {
	for {
		// Refresh 15 minutes before expiry
		a.mu.RLock()
		until := time.Until(a.expires) - 15*time.Minute
		a.mu.RUnlock()

		if until < 30*time.Second {
			until = 30 * time.Second
		}

		select {
		case <-time.After(until):
			if err := a.refresh(); err != nil {
				a.log.Error("failed to refresh GitHub App token, retrying in 30s", "error", err)
				select {
				case <-time.After(30 * time.Second):
				case <-a.done:
					return
				}
			}
		case <-a.done:
			return
		}
	}
}

func (a *AppAuth) refresh() error {
	jwt, err := a.signJWT()
	if err != nil {
		return err
	}

	token, expires, err := a.exchangeToken(jwt)
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.token = token
	a.expires = expires
	a.mu.Unlock()

	a.log.Info("GitHub App token refreshed", "expires", expires.Format(time.RFC3339))
	return nil
}

func (a *AppAuth) signJWT() (string, error) {
	now := time.Now().Add(-30 * time.Second) // clock skew buffer
	exp := now.Add(10 * time.Minute)

	header := base64url(mustJSON(map[string]string{"alg": "RS256", "typ": "JWT"}))
	payload := base64url(mustJSON(map[string]any{
		"iat": now.Unix(),
		"exp": exp.Unix(),
		"iss": a.appID,
	}))

	sigInput := header + "." + payload
	hashed := sha256.Sum256([]byte(sigInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}

	return sigInput + "." + base64url(sig), nil
}

func (a *AppAuth) exchangeToken(jwt string) (string, time.Time, error) {
	url := a.tokenURL
	if url == "" {
		url = fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", a.installationID)
	}
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("creating token exchange request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	httpClient := a.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token exchange request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("reading token response body: %w", err)
	}
	if resp.StatusCode != 201 {
		return "", time.Time{}, fmt.Errorf("GitHub API %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("parsing token response: %w", err)
	}

	expires, err := time.Parse(time.RFC3339, result.ExpiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parsing token expiry %q: %w", result.ExpiresAt, err)
	}
	return result.Token, expires, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// This only marshals simple maps with string/int values — if it fails,
		// the JWT will be malformed and the API call will fail with a clear error.
		return []byte("{}")
	}
	return b
}

func base64url(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}
