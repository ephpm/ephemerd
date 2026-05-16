//go:build !darwin

package dind

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestPushHandlerEndToEnd drives the full /auth → /push HTTP path through a
// real dind.Server backed by an embedded containerd, with a tiny synthetic
// image staged in the buildkit namespace and a mock registry that performs
// the Docker Hub-style Bearer challenge. Asserts:
//
//   - Mock registry sees Basic auth on /auth/token with the credentials
//     /auth posted to dind.
//   - Manifest is PUT to the mock registry.
//   - /push response status is 200 with no error body.
//
// This is the test that would have caught every iteration of the push bug
// (qualifyDockerHubRef, ConfigureHosts double-scope, cache miss) without
// round-tripping through GitHub Actions.
func TestPushHandlerEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping push e2e in short mode")
	}

	const (
		loginUser = "ephpm"
		loginPass = "synthetic-pat-for-test"
		repoName  = "ephpm/ephemerd"
		imageTag  = "test-tag"
	)

	// Mock registry. Returns a Bearer challenge on first /v2/... touch,
	// validates Basic auth on the token endpoint, accepts every blob /
	// manifest upload after that.
	var (
		mu              sync.Mutex
		sawTokenAuth    string
		manifestPutPath string
		blobPuts        int
	)
	mockMux := http.NewServeMux()
	mockMux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawTokenAuth = r.Header.Get("Authorization")
		mu.Unlock()
		if !strings.HasPrefix(sawTokenAuth, "Basic ") {
			http.Error(w, "expected Basic auth on token endpoint", http.StatusUnauthorized)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sawTokenAuth, "Basic "))
		if err != nil || string(raw) != loginUser+":"+loginPass {
			http.Error(w, "credentials mismatch", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "issued-token-xyz",
			"expires_in": 60,
		})
	})
	mockMux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		// Anonymous probes get the challenge.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/auth/token",service="registry.example",scope="repository:%s:push,pull"`,
					originBaseScheme(r), repoName))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Once authed, accept whatever the client sends.
		switch r.Method {
		case http.MethodHead:
			// Pretend nothing exists yet so the client uploads everything.
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPost:
			// Begin blob upload — point client at a /v2/<name>/blobs/uploads/<id>.
			w.Header().Set("Location", originBaseScheme(r)+r.URL.Path+"upload/")
			w.Header().Set("Docker-Upload-UUID", "upload")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPatch:
			w.Header().Set("Location", originBaseScheme(r)+r.URL.Path)
			w.Header().Set("Range", "0-0")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPut:
			mu.Lock()
			if strings.Contains(r.URL.Path, "/manifests/") {
				manifestPutPath = r.URL.Path
			} else {
				blobPuts++
			}
			mu.Unlock()
			w.Header().Set("Docker-Content-Digest", r.URL.Query().Get("digest"))
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mock := httptest.NewServer(mockMux)
	t.Cleanup(mock.Close)
	mockHost := mustHost(t, mock.URL)
	mockRef := mockHost + "/" + repoName + ":" + imageTag

	// Reuse the process-wide shared containerd (see testcontainerd_test.go).
	// containerd's prometheus metrics use a global registry, so spawning a
	// second containerd in the same test binary panics. The shared instance
	// is fine here because the test stages into the "buildkit" namespace
	// which doesn't collide with anything else in the suite.
	ctrdClient := sharedTestContainerd(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// Per-test scratch dir for dind's socket + job dir, separate from the
	// shared containerd's data dir.
	dataDir, err := os.MkdirTemp("", "ephemerd-push-e2e-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Logf("cleanup: remove %s: %v", dataDir, err)
		}
	})
	bkNamespace := "buildkit"
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), bkNamespace), 60*time.Second)
	defer cancel()

	// Hold a lease across the entire staging→push lifecycle. Without this,
	// content.WriteBlob registers the blob in the namespace bucket but
	// attaches no lease (addContentLease is a no-op without leases.FromContext)
	// and no GC-ref labels are written for plain child blobs. The buildkit
	// namespace can then have orphan content that is racy with respect to
	// containerd's internal flushing/visibility paths — this manifested as
	// CI flakes where TestPushHandlerEndToEnd would fail mid-push with
	// "content digest sha256:...layer...: not found".
	lease, err := ctrdClient.LeasesService().Create(ctx, leases.WithExpiration(5*time.Minute))
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	t.Cleanup(func() {
		if err := ctrdClient.LeasesService().Delete(context.Background(), lease); err != nil {
			t.Logf("delete lease: %v", err)
		}
	})
	ctx = leases.WithLease(ctx, lease.ID)

	// Stage a synthetic OCI image: empty layer + tiny config + manifest
	// pointing at both. Image record `mockRef` so /push GetImage finds it.
	imgDesc, err := stageSyntheticImage(ctx, ctrdClient, mockRef)
	if err != nil {
		t.Fatalf("stage image: %v", err)
	}
	t.Logf("staged image %s -> %s (%d bytes)", mockRef, imgDesc.Digest, imgDesc.Size)

	// Bring up the dind server.
	s, err := New(Config{
		JobID:   "push-e2e",
		DataDir: dataDir,
		Client:  ctrdClient,
		Log:     log,
	})
	if err != nil {
		t.Fatalf("dind New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("dind Start: %v", err)
	}
	t.Cleanup(s.Stop)

	cli := dialServer(s)

	// Drive POST /auth as docker login would.
	authBody := fmt.Sprintf(`{"username":%q,"password":%q,"serveraddress":%q}`,
		loginUser, loginPass, mock.URL)
	authReq, _ := http.NewRequest(http.MethodPost, "http://docker/auth", strings.NewReader(authBody))
	authReq.Header.Set("Content-Type", "application/json")
	authResp, err := cli.Do(authReq)
	if err != nil {
		t.Fatalf("POST /auth: %v", err)
	}
	_ = authResp.Body.Close()
	if authResp.StatusCode != http.StatusOK {
		t.Fatalf("/auth status = %d", authResp.StatusCode)
	}

	// Drive POST /images/<name>/push.
	pushURL := fmt.Sprintf("http://docker/images/%s/push?tag=%s",
		url.PathEscape(mockHost+"/"+repoName), imageTag)
	pushReq, _ := http.NewRequest(http.MethodPost, pushURL, nil)
	pushReq.Header.Set("X-Registry-Auth",
		base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"username":%q,"password":%q,"serveraddress":%q}`,
			loginUser, loginPass, mock.URL))))
	pushResp, err := cli.Do(pushReq)
	if err != nil {
		t.Fatalf("POST /push: %v", err)
	}
	defer func() { _ = pushResp.Body.Close() }()

	body, _ := io.ReadAll(pushResp.Body)
	t.Logf("push response (%d): %s", pushResp.StatusCode, string(body))
	if strings.Contains(string(body), `"error":`) {
		t.Fatalf("/push returned error in body:\n%s", body)
	}

	mu.Lock()
	defer mu.Unlock()
	if sawTokenAuth == "" {
		t.Error("mock registry token endpoint was never called")
	}
	if manifestPutPath == "" {
		t.Error("mock registry never received the manifest PUT")
	}
	if blobPuts == 0 {
		t.Error("mock registry never received a blob PUT")
	}
}

// stageSyntheticImage writes a minimal OCI manifest (empty layer + tiny
// config + manifest) into containerd's content store and registers an
// image record at `name`. Returns the manifest descriptor.
func stageSyntheticImage(ctx context.Context, client containerdClient, name string) (ocispec.Descriptor, error) {
	cs := client.ContentStore()

	// Non-empty synthetic layer — containerd's content store does not persist
	// zero-length blobs, so the push handler would fail to resolve the digest.
	// The registry mock accepts anything; it doesn't need to be a real tar.
	layerBytes := []byte("synthetic-layer-for-push-e2e")
	layerDigest := digest.FromBytes(layerBytes)
	layerDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    layerDigest,
		Size:      int64(len(layerBytes)),
	}
	if err := content.WriteBlob(ctx, cs, "layer-"+layerDigest.String(),
		bytes.NewReader(layerBytes), layerDesc); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("write layer: %w", err)
	}

	configBytes := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` +
		layerDigest.String() + `"]}}`)
	configDigest := digest.FromBytes(configBytes)
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    configDigest,
		Size:      int64(len(configBytes)),
	}
	if err := content.WriteBlob(ctx, cs, "config-"+configDigest.String(),
		bytes.NewReader(configBytes), configDesc); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("write config: %w", err)
	}

	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	manifest.SchemaVersion = 2
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("marshal manifest: %w", err)
	}
	manifestDigest := digest.FromBytes(manifestBytes)
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    manifestDigest,
		Size:      int64(len(manifestBytes)),
	}
	// Attach gc.ref.content labels so containerd's GC keeps the config and
	// layer reachable through the manifest. Without these the bare blobs are
	// unreferenced (the image record only points at the manifest, not its
	// children) and a GC pass between staging and push deletes the layer —
	// the resulting "content digest <layer>: not found" error is the flake
	// that made this test pass locally but fail intermittently in CI.
	manifestLabels := map[string]string{
		"containerd.io/gc.ref.content.config": configDigest.String(),
		"containerd.io/gc.ref.content.l.0":    layerDigest.String(),
	}
	if err := content.WriteBlob(ctx, cs, "manifest-"+manifestDigest.String(),
		bytes.NewReader(manifestBytes), manifestDesc, content.WithLabels(manifestLabels)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("write manifest: %w", err)
	}

	if _, err := client.ImageService().Create(ctx, images.Image{
		Name:   name,
		Target: manifestDesc,
	}); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("create image record: %w", err)
	}
	return manifestDesc, nil
}

// containerdClient is the subset of *containerd.Client we need to stage an
// image. Pulled out so the helper signature doesn't drag the full Client
// interface across files.
type containerdClient interface {
	ContentStore() content.Store
	ImageService() images.Store
}

// testSocketPath returns a unique containerd socket / pipe path for this
// test, so multiple tests (and the live daemon) can run concurrently
// without colliding on \\.\pipe\ephemerd-containerd.
func testSocketPath(t *testing.T) string {
	t.Helper()
	if goruntime.GOOS == "windows" {
		return `\\.\pipe\ephemerd-containerd-test-` + sanitizeForPipe(t.Name())
	}
	return t.TempDir() + "/containerd.sock"
}

func sanitizeForPipe(s string) string {
	return strings.NewReplacer("/", "-", `\`, "-", " ", "-", ":", "-").Replace(s)
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse mock url %q: %v", raw, err)
	}
	return u.Host
}

func originBaseScheme(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
