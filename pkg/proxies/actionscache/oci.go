package actionscache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// registry is the slice of the OCI distribution API the cache needs, spoken
// to a writable registry:2. Hand-rolled rather than go-containerregistry's
// remote package because entries are streamed in as blocks whose digest is
// not known until the last byte, and the whole surface is seven endpoints.
type registry struct {
	base *url.URL
	user string
	pass string
	hc   *http.Client
}

const (
	manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	configMediaType   = "application/vnd.ephemerd.actions-cache.config.v1+json"
	chunkMediaType    = "application/vnd.ephemerd.actions-cache.chunk.v1"
)

// errNotFound is a 404 from the registry (unknown repository, tag or blob).
var errNotFound = errors.New("not found in registry")

func newRegistry(endpoint, user, pass string, hc *http.Client) (*registry, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("registry endpoint %q is not an absolute URL", endpoint)
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &registry{base: u, user: user, pass: pass, hc: hc}, nil
}

func (r *registry) url(path string) string {
	return r.base.String() + path
}

func (r *registry) do(req *http.Request) (*http.Response, error) {
	if r.user != "" {
		req.SetBasicAuth(r.user, r.pass)
	}
	return r.hc.Do(req)
}

// status turns a non-2xx response into an error, consuming the body.
func status(resp *http.Response, what string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", what, errNotFound)
	}
	return fmt.Errorf("%s: registry answered %s: %s", what, resp.Status, strings.TrimSpace(string(body)))
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// ping is the health probe: the registry's API root, authenticated.
func (r *registry) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url("/v2/"), nil)
	if err != nil {
		return err
	}
	resp, err := r.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	return status(resp, "GET /v2/")
}

// resolve turns an upload Location (relative or absolute) into a URL.
func (r *registry) resolve(loc string) (*url.URL, error) {
	u, err := url.Parse(loc)
	if err != nil {
		return nil, fmt.Errorf("bad upload location %q: %w", loc, err)
	}
	return r.base.ResolveReference(u), nil
}

// countingHasher digests and counts what is streamed through it.
type countingHasher struct {
	h hash.Hash
	n int64
}

func (c *countingHasher) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.h.Write(p)
}

// uploadBlob streams body into the repository as one blob: start an upload,
// PATCH the bytes through (digesting them on the way), then close it with the
// digest. It never holds more than the transport's buffer in memory.
func (r *registry) uploadBlob(ctx context.Context, repo string, body io.Reader) (digest string, size int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url("/v2/"+repo+"/blobs/uploads/"), nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := r.do(req)
	if err != nil {
		return "", 0, err
	}
	drain(resp)
	if resp.StatusCode != http.StatusAccepted {
		return "", 0, fmt.Errorf("starting blob upload in %s: registry answered %s", repo, resp.Status)
	}
	loc, err := r.resolve(resp.Header.Get("Location"))
	if err != nil {
		return "", 0, err
	}

	ch := &countingHasher{h: sha256.New()}
	req, err = http.NewRequestWithContext(ctx, http.MethodPatch, loc.String(), io.TeeReader(body, ch))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err = r.do(req)
	if err != nil {
		return "", 0, err
	}
	drain(resp)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return "", 0, fmt.Errorf("streaming blob into %s: registry answered %s", repo, resp.Status)
	}
	if l := resp.Header.Get("Location"); l != "" {
		if loc, err = r.resolve(l); err != nil {
			return "", 0, err
		}
	}

	digest = "sha256:" + hex.EncodeToString(ch.h.Sum(nil))
	q := loc.Query()
	q.Set("digest", digest)
	loc.RawQuery = q.Encode()
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, loc.String(), http.NoBody)
	if err != nil {
		return "", 0, err
	}
	resp, err = r.do(req)
	if err != nil {
		return "", 0, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated {
		return "", 0, status(resp, "committing blob in "+repo)
	}
	return digest, ch.n, nil
}

// putManifest stores body under tag and returns the digest the registry
// filed it under.
func (r *registry) putManifest(ctx context.Context, repo, tag string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, r.url("/v2/"+repo+"/manifests/"+tag), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", manifestMediaType)
	resp, err := r.do(req)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if err := status(resp, "PUT manifest "+repo+":"+tag); err != nil {
		return "", err
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		return d, nil
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// getManifest fetches a manifest by tag or digest.
func (r *registry) getManifest(ctx context.Context, repo, ref string) (body []byte, digest string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url("/v2/"+repo+"/manifests/"+ref), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", manifestMediaType)
	resp, err := r.do(req)
	if err != nil {
		return nil, "", err
	}
	defer drain(resp)
	if err := status(resp, "GET manifest "+repo+":"+ref); err != nil {
		return nil, "", err
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", err
	}
	digest = resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	return body, digest, nil
}

// deleteManifest removes a manifest by digest. The registry needs
// REGISTRY_STORAGE_DELETE_ENABLED=true; blobs are reclaimed later by its
// garbage collector (run from mayfly's timer, not from here).
func (r *registry) deleteManifest(ctx context.Context, repo, digest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, r.url("/v2/"+repo+"/manifests/"+digest), nil)
	if err != nil {
		return err
	}
	resp, err := r.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if err := status(resp, "DELETE manifest "+repo+"@"+digest); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	return nil
}

// listTags returns every tag in repo; an unknown repository is empty, not an
// error (no job has saved under that ref yet).
func (r *registry) listTags(ctx context.Context, repo string) ([]string, error) {
	var tags []string
	next := "/v2/" + repo + "/tags/list?n=1000"
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url(next), nil)
		if err != nil {
			return nil, err
		}
		resp, err := r.do(req)
		if err != nil {
			return nil, err
		}
		if err := status(resp, "GET tags "+repo); err != nil {
			drain(resp)
			if errors.Is(err, errNotFound) {
				return nil, nil
			}
			return nil, err
		}
		var page struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page)
		drain(resp)
		if err != nil {
			return nil, fmt.Errorf("decoding tags of %s: %w", repo, err)
		}
		tags = append(tags, page.Tags...)
		next = nextLink(resp.Header.Get("Link"))
	}
	return tags, nil
}

// catalog returns the repositories whose name starts with prefix.
func (r *registry) catalog(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	next := "/v2/_catalog?n=1000"
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url(next), nil)
		if err != nil {
			return nil, err
		}
		resp, err := r.do(req)
		if err != nil {
			return nil, err
		}
		if err := status(resp, "GET catalog"); err != nil {
			drain(resp)
			return nil, err
		}
		var page struct {
			Repositories []string `json:"repositories"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page)
		drain(resp)
		if err != nil {
			return nil, fmt.Errorf("decoding catalog: %w", err)
		}
		for _, name := range page.Repositories {
			if strings.HasPrefix(name, prefix) {
				out = append(out, name)
			}
		}
		next = nextLink(resp.Header.Get("Link"))
	}
	return out, nil
}

// nextLink extracts the path of an RFC 5988 rel="next" link, the way
// registry:2 paginates tags and the catalog.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start, end := strings.Index(part, "<"), strings.Index(part, ">")
		if start < 0 || end <= start {
			return ""
		}
		link := part[start+1 : end]
		if u, err := url.Parse(link); err == nil && u.IsAbs() {
			link = u.RequestURI()
		}
		return link
	}
	return ""
}

// openBlob streams n bytes of a blob starting at off (n < 0: to the end).
func (r *registry) openBlob(ctx context.Context, repo, digest string, off, n int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url("/v2/"+repo+"/blobs/"+digest), nil)
	if err != nil {
		return nil, err
	}
	if off > 0 || n >= 0 {
		rng := "bytes=" + strconv.FormatInt(off, 10) + "-"
		if n >= 0 {
			rng += strconv.FormatInt(off+n-1, 10)
		}
		req.Header.Set("Range", rng)
	}
	resp, err := r.do(req)
	if err != nil {
		return nil, err
	}
	if err := status(resp, "GET blob "+repo+"@"+digest); err != nil {
		drain(resp)
		return nil, err
	}
	if (off > 0 || n >= 0) && resp.StatusCode != http.StatusPartialContent {
		// A registry that ignores Range sends the whole blob: skip to off and
		// bound it ourselves rather than serve the wrong bytes.
		if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
			drain(resp)
			return nil, fmt.Errorf("skipping to offset %d of %s: %w", off, digest, err)
		}
		if n >= 0 {
			return struct {
				io.Reader
				io.Closer
			}{io.LimitReader(resp.Body, n), resp.Body}, nil
		}
	}
	return resp.Body, nil
}
