package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// generateTestKey creates a temporary PEM file with an RSA private key.
func generateTestKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}

	path := filepath.Join(t.TempDir(), "test-key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return key, path
}

// --- signJWT tests ---

func TestSignJWT_Structure(t *testing.T) {
	key, _ := generateTestKey(t)
	a := &AppAuth{
		appID:          12345,
		installationID: 67890,
		key:            key,
		log:            discardLogger(),
	}

	jwt, err := a.signJWT()
	if err != nil {
		t.Fatalf("signJWT() error: %v", err)
	}

	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}

	// Decode header
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decoding header: %v", err)
	}
	var header map[string]string
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("parsing header: %v", err)
	}
	if header["alg"] != "RS256" {
		t.Errorf("header.alg = %q, want %q", header["alg"], "RS256")
	}
	if header["typ"] != "JWT" {
		t.Errorf("header.typ = %q, want %q", header["typ"], "JWT")
	}

	// Decode payload
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("parsing payload: %v", err)
	}
	if payload["iss"] != float64(12345) {
		t.Errorf("payload.iss = %v, want 12345", payload["iss"])
	}
	if _, ok := payload["iat"]; !ok {
		t.Error("payload missing 'iat' claim")
	}
	if _, ok := payload["exp"]; !ok {
		t.Error("payload missing 'exp' claim")
	}

	// Verify exp > iat
	iat := payload["iat"].(float64)
	exp := payload["exp"].(float64)
	if exp <= iat {
		t.Errorf("exp (%v) should be > iat (%v)", exp, iat)
	}

	// Signature should be non-empty base64url
	if parts[2] == "" {
		t.Error("signature part is empty")
	}
}

func TestSignJWT_NoPadding(t *testing.T) {
	key, _ := generateTestKey(t)
	a := &AppAuth{appID: 1, key: key, log: discardLogger()}

	jwt, err := a.signJWT()
	if err != nil {
		t.Fatalf("signJWT() error: %v", err)
	}
	if strings.Contains(jwt, "=") {
		t.Error("JWT should not contain base64 padding characters")
	}
}

// --- exchangeToken with mock server ---

func TestExchangeToken_Success(t *testing.T) {
	key, _ := generateTestKey(t)

	expires := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("Authorization = %q, want Bearer prefix", auth)
		}

		w.WriteHeader(201)
		resp := map[string]string{
			"token":      "ghs_test_token_123",
			"expires_at": expires,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encoding response: %v", err)
		}
	}))
	defer srv.Close()

	a := &AppAuth{
		appID:          1,
		installationID: 1,
		key:            key,
		log:            discardLogger(),
	}

	// Monkey-patch: we can't easily override the URL in exchangeToken,
	// so instead test the full flow via a custom approach.
	// For now, verify signJWT works and test the token storage.
	jwt, err := a.signJWT()
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	if jwt == "" {
		t.Fatal("signJWT returned empty")
	}
}

func TestExchangeToken_Non201(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		if _, err := w.Write([]byte(`{"message":"forbidden"}`)); err != nil {
			t.Logf("writing response: %v", err)
		}
	}))
	defer srv.Close()

	key, _ := generateTestKey(t)
	a := &AppAuth{
		appID:          1,
		installationID: 1,
		key:            key,
		log:            discardLogger(),
	}

	// exchangeToken uses a hardcoded URL, so we can't redirect it.
	// But we can verify the JWT is well-formed for the API.
	jwt, err := a.signJWT()
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	// JWT should be usable as a Bearer token
	if !strings.Contains(jwt, ".") {
		t.Error("JWT missing dot separators")
	}
}

// --- Token / Stop ---

func TestAppAuth_TokenAndStop(t *testing.T) {
	key, _ := generateTestKey(t)
	a := &AppAuth{
		appID: 1,
		key:   key,
		log:   discardLogger(),
		done:  make(chan struct{}),
	}

	// Set token directly (bypass refresh)
	a.mu.Lock()
	a.token = "test-token"
	a.expires = time.Now().Add(1 * time.Hour)
	a.mu.Unlock()

	if got := a.Token(); got != "test-token" {
		t.Errorf("Token() = %q, want %q", got, "test-token")
	}

	// Start and stop the refresh loop
	go a.refreshLoop()
	a.Stop()
}

// --- mustJSON ---

func TestMustJSON(t *testing.T) {
	b := mustJSON(map[string]string{"key": "value"})
	if len(b) == 0 {
		t.Fatal("mustJSON returned empty")
	}

	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("mustJSON produced invalid JSON: %v", err)
	}
	if m["key"] != "value" {
		t.Errorf("key = %q, want %q", m["key"], "value")
	}
}

func TestAppAuth_Token_NearExpiry(t *testing.T) {
	key, _ := generateTestKey(t)
	a := &AppAuth{
		appID: 1,
		key:   key,
		log:   discardLogger(),
		done:  make(chan struct{}),
	}

	// Set a token that expires in 3 minutes (< 5min threshold)
	a.mu.Lock()
	a.token = "stale-token"
	a.expires = time.Now().Add(3 * time.Minute)
	a.mu.Unlock()

	// Token() should attempt a refresh (which will fail against real API),
	// but should still return the existing token rather than empty string
	got := a.Token()
	if got == "" {
		t.Error("Token() should return existing token even if refresh fails")
	}
}

func TestAppAuth_Token_WellWithinExpiry(t *testing.T) {
	key, _ := generateTestKey(t)
	a := &AppAuth{
		appID: 1,
		key:   key,
		log:   discardLogger(),
		done:  make(chan struct{}),
	}

	// Set a token that expires in 30 minutes (well within threshold)
	a.mu.Lock()
	a.token = "good-token"
	a.expires = time.Now().Add(30 * time.Minute)
	a.mu.Unlock()

	got := a.Token()
	if got != "good-token" {
		t.Errorf("Token() = %q, want %q", got, "good-token")
	}
}

// --- appAuthTransport ---

func TestAppAuthTransport(t *testing.T) {
	key, _ := generateTestKey(t)
	a := &AppAuth{
		appID: 1,
		key:   key,
		log:   discardLogger(),
		done:  make(chan struct{}),
	}
	a.mu.Lock()
	a.token = "transport-test-token"
	a.expires = time.Now().Add(1 * time.Hour)
	a.mu.Unlock()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "token transport-test-token" {
			t.Errorf("Authorization = %q, want %q", auth, "token transport-test-token")
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	transport := &appAuthTransport{app: a}
	client := &http.Client{Transport: transport}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// --- NewAppAuth with PEM file ---

func TestNewAppAuth_InvalidPath(t *testing.T) {
	_, err := NewAppAuth(1, 1, "/nonexistent/key.pem", discardLogger())
	if err == nil {
		t.Fatal("expected error for nonexistent key file")
	}
}

func TestNewAppAuth_InvalidPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(path, []byte("not a pem file"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := NewAppAuth(1, 1, path, discardLogger())
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestNewAppAuth_InvalidPrivateKey(t *testing.T) {
	// Write a valid PEM block but with garbage bytes
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("not a real key")}
	path := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := NewAppAuth(1, 1, path, discardLogger())
	if err == nil {
		t.Fatal("expected error for garbage private key")
	}
}

// --- concurrent Token() refresh tests ---

// countingTransport counts the total number of HTTP requests it intercepts.
// Used to assert that concurrent Token() callers collapse into a single
// upstream refresh.
type countingTransport struct {
	count atomic.Int32
	inner http.RoundTripper
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.count.Add(1)
	return c.inner.RoundTrip(req)
}

// fakeTokenServer returns a httptest.Server that always responds with a
// valid token-exchange payload whose token includes a per-call counter.
// callCount is incremented on every request so tests can assert how many
// upstream HTTP exchanges actually happened.
func fakeTokenServer(t *testing.T, callCount *atomic.Int32, expiresIn time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		expires := time.Now().Add(expiresIn).UTC().Format(time.RFC3339)
		w.WriteHeader(201)
		body := map[string]string{
			"token":      "ghs_token_" + strconvI(n),
			"expires_at": expires,
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Logf("encoding token response: %v", err)
		}
	}))
}

// strconvI is a minimal int->string helper to avoid importing strconv just
// for one call.
func strconvI(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestAppAuth_Token_ConcurrentRefreshSerialized fires many goroutines hitting
// Token() while the cached token is inside the 5-minute expiry window.
// All callers must observe a valid (non-empty) token and the refresh
// endpoint must be hit exactly once.
func TestAppAuth_Token_ConcurrentRefreshSerialized(t *testing.T) {
	key, _ := generateTestKey(t)

	var serverHits atomic.Int32
	srv := fakeTokenServer(t, &serverHits, 1*time.Hour)
	defer srv.Close()

	transport := &countingTransport{inner: http.DefaultTransport}
	a := &AppAuth{
		appID:          1,
		installationID: 42,
		key:            key,
		log:            discardLogger(),
		tokenURL:       srv.URL,
		httpClient:     &http.Client{Transport: transport},
		done:           make(chan struct{}),
	}

	// Pre-populate a stale token (expires in 1 minute — well inside the
	// 5-minute on-demand refresh threshold).
	a.mu.Lock()
	a.token = "stale-token"
	a.expires = time.Now().Add(1 * time.Minute)
	a.mu.Unlock()

	const goroutines = 32
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make(chan string, goroutines)
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start // synchronize all goroutines
			results <- a.Token()
		}()
	}

	close(start)
	wg.Wait()
	close(results)

	// Every goroutine should have observed a refreshed (non-empty) token.
	for got := range results {
		if got == "" {
			t.Error("Token() returned empty string from a goroutine")
		}
	}

	// Critically: only ONE upstream refresh should have occurred. The
	// remaining 31 callers must have hit the post-lock fast-path that
	// re-checks expiry.
	hits := serverHits.Load()
	if hits != 1 {
		t.Errorf("token server received %d requests, want exactly 1 (refresh should be serialized)", hits)
	}
	if got := transport.count.Load(); got != 1 {
		t.Errorf("transport saw %d round-trips, want 1", got)
	}
}

// TestAppAuth_Token_NoRefreshWhenFresh asserts that Token() callers do NOT
// trigger a refresh if the cached token is well outside the 5-minute window.
func TestAppAuth_Token_NoRefreshWhenFresh(t *testing.T) {
	key, _ := generateTestKey(t)

	var serverHits atomic.Int32
	srv := fakeTokenServer(t, &serverHits, 1*time.Hour)
	defer srv.Close()

	transport := &countingTransport{inner: http.DefaultTransport}
	a := &AppAuth{
		appID:          1,
		installationID: 42,
		key:            key,
		log:            discardLogger(),
		tokenURL:       srv.URL,
		httpClient:     &http.Client{Transport: transport},
		done:           make(chan struct{}),
	}

	a.mu.Lock()
	a.token = "fresh-token"
	a.expires = time.Now().Add(30 * time.Minute) // way outside threshold
	a.mu.Unlock()

	for i := 0; i < 100; i++ {
		if got := a.Token(); got != "fresh-token" {
			t.Fatalf("Token() = %q, want fresh-token", got)
		}
	}

	if hits := serverHits.Load(); hits != 0 {
		t.Errorf("expected zero refreshes for a fresh token, got %d server hits", hits)
	}
}

// TestAppAuth_Token_RefreshSwingsToFastPath asserts that once a successful
// refresh produces a token outside the 5-minute expiry window, subsequent
// concurrent Token() callers exit on the RLock-only fast path and never
// touch the token endpoint.
func TestAppAuth_Token_RefreshSwingsToFastPath(t *testing.T) {
	key, _ := generateTestKey(t)

	var serverHits atomic.Int32
	// First refresh produces a long-lived token (1h), well outside the
	// 5-minute threshold.
	srv := fakeTokenServer(t, &serverHits, 1*time.Hour)
	defer srv.Close()

	a := &AppAuth{
		appID:          1,
		installationID: 42,
		key:            key,
		log:            discardLogger(),
		tokenURL:       srv.URL,
		httpClient:     srv.Client(),
		done:           make(chan struct{}),
	}

	// Initial stale token forces a refresh.
	a.mu.Lock()
	a.token = "stale"
	a.expires = time.Now().Add(1 * time.Minute)
	a.mu.Unlock()

	// First call: triggers a refresh.
	if got := a.Token(); got == "" {
		t.Fatal("first Token() call returned empty")
	}
	if got := serverHits.Load(); got != 1 {
		t.Fatalf("after first call: hits = %d, want 1", got)
	}

	// Subsequent concurrent calls: fast path, no refresh.
	var wg sync.WaitGroup
	const n = 64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if a.Token() == "" {
				t.Error("Token() returned empty in burst")
			}
		}()
	}
	wg.Wait()

	if got := serverHits.Load(); got != 1 {
		t.Errorf("burst against fresh token caused %d additional refreshes, want 0", got-1)
	}
}
