//go:build !darwin

package dind

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestImagePull_SharedNamespaceHitIsUsableFromJobNamespace is the regression
// test for the "content digest sha256:...: not found" failure that broke
// every GHA `container:` job whose image happened to also live in the shared
// "ephemerd" namespace (the runtime pulls runner images there).
//
// The old handleImagePull short-circuited on
// client.GetImage(sharedCtx, ref) and then used that handle with the job
// namespace: registering the Image record in the job namespace, reporting
// the pull as successful. containerd's content store is namespaced —
// metadata.contentStore.checkAccess only looks at the blob bucket of the
// namespace on the context — so the job namespace could not read the
// manifest or config the record pointed at. The next /containers/create
// died inside client.WithNewSnapshot (which reads the image config to get
// the rootfs chain ID) with:
//
//	creating container: content digest sha256:<config>: not found
//
// The shape below reproduces that exactly: stage the image in the shared
// namespace first, serve the same image from a mock registry, drive
// POST /images/create, then assert the job namespace can actually use what
// the handler claims it pulled.
//
// # Privilege requirements
//
// Unpacking a layer needs mount(2): containerd's walking differ bind-mounts
// the snapshot directory into a scratch dir (mount.WithTempMount) and untars
// into it, which takes CAP_SYS_ADMIN. ephemerd's own CI runs inside an
// ephemerd job container as an unprivileged uid with the default OCI
// capability set, so it cannot. The test therefore splits in two:
//
//   - Tier one — "the handler went to the registry". Needs no privileges and
//     runs everywhere. This is the assertion that fails on the pre-fix code:
//     the old fast path resolved the ref out of the shared namespace and
//     never contacted a registry at all, so manifestReqs stays 0. It survives
//     a denied mount because containerd fetches before it unpacks.
//   - Tier two — "and what it pulled is usable from the job namespace":
//     record present, manifest readable, RootFS resolvable, image unpacked.
//     This is the direct evidence for the reported failure, and it needs the
//     mount privilege, because containerd's Pull is atomic over the unpack —
//     a failed unpack leaves no image record to inspect. It skips loudly
//     where mount(2) is denied; see probeMountPrivilege.
//
// So in unprivileged CI this test proves the fast path is gone but not that
// the result is usable. To get both, run it as root on a Linux host with
// overlayfs:
//
//	sudo go test -tags containers_image_openpgp -run TestImagePull_Shared -v ./pkg/dind/
func TestImagePull_SharedNamespaceHitIsUsableFromJobNamespace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pull e2e in short mode")
	}
	if goruntime.GOOS != "linux" {
		// WithPullUnpack needs the overlayfs snapshotter.
		t.Skipf("image unpack requires the overlayfs snapshotter; goos=%s", goruntime.GOOS)
	}

	// Decide up front whether the unpack tier can run at all. Probing rather
	// than checking euid==0 keeps this honest in containers, user namespaces
	// and anywhere else the capability and the uid disagree.
	mountErr := probeMountPrivilege(t)
	canUnpack := mountErr == nil
	if !canUnpack {
		// go test is not verbose in CI (`mage test` runs plain `go test`), so
		// t.Log alone would make this degradation invisible on a green run.
		// Say it on stderr, which the CI log does capture.
		fmt.Fprintf(os.Stderr,
			"[pull-e2e] DEGRADED: cannot mount(2) here (%v) — skipping "+
				"image_is_usable_from_job_namespace. Only the registry-was-contacted "+
				"assertion runs; it still fails on the pre-fix code, but nothing here "+
				"proves the pulled image is usable. Run as root on a Linux host with "+
				"overlayfs for the full test.\n", mountErr)
		t.Logf("mount(2) unavailable (%v); tier two will skip", mountErr)
	}

	const (
		repoName = "ephpm/pull-e2e"
		imageTag = "v1"
	)

	blobs, manifestDesc, configDigest := syntheticImageBlobs(t)

	// Mock registry. Anonymous, no auth challenge — handleImagePull uses
	// containerd's default resolver, which talks plain HTTP to loopback
	// hosts (docker.MatchLocalhost), so no TLS setup is needed.
	var manifestReqs, blobGets int
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v2/")
		switch {
		case path == "":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(path, "/manifests/"):
			manifestReqs++
			w.Header().Set("Content-Type", manifestDesc.MediaType)
			w.Header().Set("Docker-Content-Digest", manifestDesc.Digest.String())
			w.Header().Set("Content-Length", strconv.FormatInt(manifestDesc.Size, 10))
			if r.Method == http.MethodHead {
				return
			}
			_, _ = w.Write(blobs[manifestDesc.Digest.String()])
		case strings.Contains(path, "/blobs/"):
			dgst := path[strings.LastIndexByte(path, '/')+1:]
			body, ok := blobs[dgst]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method == http.MethodHead {
				return
			}
			blobGets++
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)

	mockHost := mustHost(t, mock.URL)
	fromImage := mockHost + "/" + repoName
	ref := fromImage + ":" + imageTag

	ctrdClient := sharedTestContainerd(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Stage the identical image in the shared namespace. This is what the
	// runtime does for runner images, and it is what made the old fast path
	// fire — without it the handler would have taken the (working) pull
	// path and the test would prove nothing.
	sharedCtx, cancel := context.WithTimeout(
		namespaces.WithNamespace(context.Background(), sharedNamespace), 90*time.Second)
	defer cancel()
	lease, err := ctrdClient.LeasesService().Create(sharedCtx, leases.WithExpiration(10*time.Minute))
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	t.Cleanup(func() {
		ctx := namespaces.WithNamespace(context.Background(), sharedNamespace)
		if err := ctrdClient.LeasesService().Delete(ctx, lease); err != nil {
			t.Logf("delete lease: %v", err)
		}
	})
	stageCtx := leases.WithLease(sharedCtx, lease.ID)
	if err := stageImageBlobs(stageCtx, ctrdClient.ContentStore(), blobs, manifestDesc, configDigest); err != nil {
		t.Fatalf("stage blobs in %s: %v", sharedNamespace, err)
	}
	if _, err := ctrdClient.ImageService().Create(stageCtx, images.Image{
		Name:   ref,
		Target: manifestDesc,
	}); err != nil {
		t.Fatalf("stage image record in %s: %v", sharedNamespace, err)
	}
	t.Cleanup(func() {
		ctx := namespaces.WithNamespace(context.Background(), sharedNamespace)
		if err := ctrdClient.ImageService().Delete(ctx, ref); err != nil {
			t.Logf("delete staged image: %v", err)
		}
	})
	// Sanity: the fast path the bug rode in on really is reachable.
	if _, err := ctrdClient.GetImage(sharedCtx, ref); err != nil {
		t.Fatalf("staged image not resolvable in %s: %v", sharedNamespace, err)
	}

	dataDir, err := os.MkdirTemp("", "ephemerd-pull-e2e-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Logf("cleanup: remove %s: %v", dataDir, err)
		}
	})

	s, err := New(Config{
		JobID:   "pull-e2e",
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
	// Stop cleans the job namespace, so it has to run after the assertions.
	t.Cleanup(s.Stop)

	// docker pull <mockhost>/ephpm/pull-e2e:v1
	cli := dialServer(s)
	pullURL := fmt.Sprintf("http://docker/images/create?fromImage=%s&tag=%s", fromImage, imageTag)
	resp, err := cli.Post(pullURL, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /images/create: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	t.Logf("pull response (%d): %s", resp.StatusCode, string(body))
	if strings.Contains(string(body), "Error:") {
		// A pull failure is fatal — with one exception. Where mount(2) is
		// denied the pull dies in its unpack step, which is a privilege
		// problem, not a namespace one; the tier-one assertion below is about
		// what the handler *did*, not what it produced, so it still holds.
		// Nothing else is tolerated: a 404, a digest mismatch, a resolve
		// failure or an unpack failure on a host that can mount stay fatal.
		switch {
		case canUnpack:
			t.Fatalf("/images/create reported an error:\n%s", body)
		case !isUnpackMountFailure(string(body)):
			t.Fatalf("/images/create failed for a reason unrelated to the missing "+
				"mount privileges (%v):\n%s", mountErr, body)
		default:
			t.Logf("pull failed at unpack for want of mount privileges (%v); "+
				"tier two will skip, tier one still applies", mountErr)
		}
	}

	// Tier one — runs everywhere, no privileges needed, and this alone is what
	// fails on the pre-fix code.
	//
	// The pre-fix handler resolved the ref out of the shared namespace and
	// returned that handle without ever talking to a registry. So "the
	// registry was contacted" is a direct, privilege-free observation that the
	// fast path is gone and the ref was pulled into the job namespace. It
	// holds even when the unpack cannot run: containerd fetches before it
	// unpacks.
	//
	// Layer/config GETs are expected to be zero: containerd's default bolt
	// content sharing policy ("shared") lets the ingest short-circuit on a
	// digest already in the backing content store, so the re-pull costs a
	// resolve round-trip and an unpack, not bandwidth. That is a containerd
	// default rather than a guarantee, so it is logged, not asserted.
	if manifestReqs == 0 {
		t.Fatal("registry was never contacted; the handler resolved the image " +
			"from another namespace instead of pulling into the job namespace")
	}
	t.Logf("registry traffic: %d manifest request(s), %d blob GET(s)", manifestReqs, blobGets)

	// Tier two — needs mount(2). Everything /containers/create actually
	// consumes, i.e. proof that the pull produced a usable image and not just
	// a well-intentioned round-trip. containerd's Pull is atomic over the
	// unpack: when the unpack fails it returns an error and leaves no image
	// record behind, so none of this is observable without the privilege.
	t.Run("image_is_usable_from_job_namespace", func(t *testing.T) {
		if !canUnpack {
			t.Skipf("requires overlayfs mount privileges (CAP_SYS_ADMIN); this "+
				"environment cannot mount(2): %v — run `sudo go test -tags "+
				"containers_image_openpgp -run TestImagePull_Shared ./pkg/dind/` "+
				"on a Linux host with overlayfs to exercise it", mountErr)
		}

		jobCtx := namespaces.WithNamespace(context.Background(), s.jobNamespace)

		img, err := ctrdClient.GetImage(jobCtx, ref)
		if err != nil {
			t.Fatalf("image %q not registered in job namespace %s: %v", ref, s.jobNamespace, err)
		}
		if _, err := ctrdClient.ContentStore().ReaderAt(jobCtx, manifestDesc); err != nil {
			t.Fatalf("manifest not readable from job namespace (the bug): %v", err)
		}
		// RootFS reads the config blob — this is the exact call inside
		// client.WithNewSnapshot that produced "content digest ...: not found".
		diffIDs, err := img.RootFS(jobCtx)
		if err != nil {
			t.Fatalf("RootFS from job namespace (the bug): %v", err)
		}
		if len(diffIDs) != 1 {
			t.Errorf("diffIDs = %v, want 1 layer", diffIDs)
		}
		unpacked, err := img.IsUnpacked(jobCtx, "overlayfs")
		if err != nil {
			t.Fatalf("IsUnpacked: %v", err)
		}
		if !unpacked {
			t.Error("image is not unpacked in the job namespace; WithNewSnapshot would have no parent to prepare from")
		}

		// The handler's own view of the world should agree.
		s.mu.Lock()
		entry, ok := s.images[ref]
		s.mu.Unlock()
		if !ok {
			t.Fatalf("no image entry recorded for %q; have %v", ref, imageKeys(s))
		}
		if entry.ID != manifestDesc.Digest.String() {
			t.Errorf("image entry ID = %s, want %s", entry.ID, manifestDesc.Digest)
		}
	})
}

// probeMountPrivilege reports nil when this process can perform the mount(2)
// call containerd's differ needs to unpack a layer, and the mount error
// otherwise.
//
// It probes with exactly the operation that fails in an unprivileged CI
// container: a bind mount, via containerd's own mount package. containerd's
// walking differ hands the snapshot's mounts to mount.WithTempMount and untars
// into the result; a single-layer snapshot on the overlayfs snapshotter is a
// plain bind mount, which is why the CI failure reads
//
//	failed to mount /tmp/containerd-mount...: ... fstype: bind ...:
//	  operation not permitted
//
// That also rules out swapping in the "native" snapshotter to dodge the
// requirement: native returns bind mounts too, so it needs the same privilege.
// The only ways to run this tier are real CAP_SYS_ADMIN or a user namespace.
//
// Probing beats checking os.Geteuid() == 0: capabilities, user namespaces,
// LSM policy and read-only mount propagation all decide this independently of
// the uid, and ephemerd's own runners are containers where they disagree.
func probeMountPrivilege(t *testing.T) error {
	t.Helper()

	src, dst := t.TempDir(), t.TempDir()
	m := mount.Mount{Type: "bind", Source: src, Options: []string{"rbind", "rw"}}
	if err := m.Mount(dst); err != nil {
		return err
	}
	if err := mount.UnmountAll(dst, 0); err != nil {
		// Leaving a stray mount behind would break t.TempDir's cleanup, so
		// this is worth failing on rather than treating as "unprivileged".
		t.Fatalf("probe: bind mount succeeded but unmount of %s failed: %v", dst, err)
	}
	return nil
}

// isUnpackMountFailure reports whether a /images/create progress stream failed
// specifically because the layer could not be unpacked for want of mount
// privileges — as opposed to any other pull failure, which must stay fatal.
func isUnpackMountFailure(body string) bool {
	unpackStage := strings.Contains(body, "failed to extract layer") ||
		strings.Contains(body, "failed to unpack image")
	denied := strings.Contains(body, "operation not permitted") ||
		strings.Contains(body, "permission denied")
	return unpackStage && denied
}

func imageKeys(s *Server) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.images))
	for k := range s.images {
		out = append(out, k)
	}
	return out
}

// syntheticImageBlobs builds a one-layer OCI image: a real gzipped tar so
// containerd's applier can unpack it, a config whose platform matches the
// host (so the default platform matcher accepts it), and a manifest tying
// them together. Returns the blobs keyed by digest string, the manifest
// descriptor, and the config digest.
func syntheticImageBlobs(t *testing.T) (map[string][]byte, ocispec.Descriptor, digest.Digest) {
	t.Helper()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	marker := []byte("ephemerd dind pull e2e\n")
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "pull-e2e-marker",
		Mode:     0o644,
		Size:     int64(len(marker)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(marker); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	diffID := digest.FromBytes(tarBuf.Bytes())

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(tarBuf.Bytes()); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	layerBytes := gzBuf.Bytes()
	layerDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    digest.FromBytes(layerBytes),
		Size:      int64(len(layerBytes)),
	}

	configBytes, err := json.Marshal(ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: goruntime.GOARCH},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{diffID}},
		Config:   ocispec.ImageConfig{Cmd: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(configBytes),
		Size:      int64(len(configBytes)),
	}

	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	manifest.SchemaVersion = 2
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestBytes),
		Size:      int64(len(manifestBytes)),
	}

	return map[string][]byte{
		layerDesc.Digest.String():    layerBytes,
		configDesc.Digest.String():   configBytes,
		manifestDesc.Digest.String(): manifestBytes,
	}, manifestDesc, configDesc.Digest
}

// stageImageBlobs writes the manifest, config and layer into the content
// store of whatever namespace ctx carries, with the gc.ref labels that keep
// the children reachable through the manifest.
func stageImageBlobs(ctx context.Context, cs content.Store, blobs map[string][]byte, manifestDesc ocispec.Descriptor, configDigest digest.Digest) error {
	var manifest ocispec.Manifest
	if err := json.Unmarshal(blobs[manifestDesc.Digest.String()], &manifest); err != nil {
		return fmt.Errorf("unmarshal manifest: %w", err)
	}

	for _, desc := range append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...) {
		if err := content.WriteBlob(ctx, cs, "stage-"+desc.Digest.String(),
			bytes.NewReader(blobs[desc.Digest.String()]), desc); err != nil {
			return fmt.Errorf("write %s: %w", desc.Digest, err)
		}
	}

	labels := map[string]string{
		"containerd.io/gc.ref.content.config": configDigest.String(),
	}
	for i, l := range manifest.Layers {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = l.Digest.String()
	}
	if err := content.WriteBlob(ctx, cs, "stage-"+manifestDesc.Digest.String(),
		bytes.NewReader(blobs[manifestDesc.Digest.String()]), manifestDesc,
		content.WithLabels(labels)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}
