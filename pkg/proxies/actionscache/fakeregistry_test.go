package actionscache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeRegistry is the subset of registry:2 the cache uses, in memory. Like
// the real one it refuses a manifest whose config or layers are missing, so
// a test cannot pass by writing manifests that point at nothing.
type fakeRegistry struct {
	t        *testing.T
	user     string
	pass     string
	mu       sync.Mutex
	blobs    map[string][]byte            // digest -> bytes (global, like a shared store)
	repoBlob map[string]map[string]bool   // repo -> digests linked into it
	uploads  map[string]*strings.Builder  // upload id -> data
	uploadTo map[string]string            // upload id -> repo
	tags     map[string]map[string]string // repo -> tag -> manifest digest
	mans     map[string][]byte            // manifest digest -> body
	nextID   int
	down     bool
	patches  int
}

func newFakeRegistry(t *testing.T) (*fakeRegistry, *httptest.Server) {
	f := &fakeRegistry{
		t: t, user: "cache", pass: "s3cret",
		blobs: map[string][]byte{}, repoBlob: map[string]map[string]bool{},
		uploads: map[string]*strings.Builder{}, uploadTo: map[string]string{},
		tags: map[string]map[string]string{}, mans: map[string][]byte{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if u, p, ok := r.BasicAuth(); !ok || u != f.user || p != f.pass {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		http.Error(w, "auth", http.StatusUnauthorized)
		return
	}
	path := r.URL.Path
	switch {
	case path == "/v2/":
		w.WriteHeader(http.StatusOK)
	case path == "/v2/_catalog":
		var names []string
		for repo := range f.tags {
			names = append(names, repo)
		}
		sort.Strings(names)
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": names})
	case strings.HasSuffix(path, "/blobs/uploads/") && r.Method == http.MethodPost:
		repo := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/"), "/blobs/uploads/")
		f.nextID++
		id := strconv.Itoa(f.nextID)
		f.uploads[id] = &strings.Builder{}
		f.uploadTo[id] = repo
		w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id+"?_state=x")
		w.WriteHeader(http.StatusAccepted)
	case strings.Contains(path, "/blobs/uploads/") && r.Method == http.MethodPatch:
		id := path[strings.LastIndex(path, "/")+1:]
		b, ok := f.uploads[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.patches++
		data, _ := io.ReadAll(r.Body)
		b.Write(data)
		w.Header().Set("Location", path+"?_state=y")
		w.WriteHeader(http.StatusAccepted)
	case strings.Contains(path, "/blobs/uploads/") && r.Method == http.MethodPut:
		id := path[strings.LastIndex(path, "/")+1:]
		b, ok := f.uploads[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		data := []byte(b.String())
		want := r.URL.Query().Get("digest")
		if got := digestOf(data); got != want {
			http.Error(w, "digest mismatch: got "+got+" want "+want, http.StatusBadRequest)
			return
		}
		repo := f.uploadTo[id]
		f.blobs[want] = data
		if f.repoBlob[repo] == nil {
			f.repoBlob[repo] = map[string]bool{}
		}
		f.repoBlob[repo][want] = true
		delete(f.uploads, id)
		w.WriteHeader(http.StatusCreated)
	case strings.Contains(path, "/blobs/") && r.Method == http.MethodGet:
		repo, dig, _ := strings.Cut(strings.TrimPrefix(path, "/v2/"), "/blobs/")
		if !f.repoBlob[repo][dig] {
			http.NotFound(w, r)
			return
		}
		data := f.blobs[dig]
		if rh := r.Header.Get("Range"); rh != "" {
			s, e, ok := parseRange(rh, int64(len(data)))
			if !ok {
				http.Error(w, "range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[s : e+1])
			return
		}
		_, _ = w.Write(data)
	case strings.HasSuffix(path, "/tags/list"):
		repo := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/"), "/tags/list")
		tags, ok := f.tags[repo]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"NAME_UNKNOWN"}]}`))
			return
		}
		var names []string
		for t := range tags {
			names = append(names, t)
		}
		sort.Strings(names)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": names})
	case strings.Contains(path, "/manifests/"):
		repo, ref, _ := strings.Cut(strings.TrimPrefix(path, "/v2/"), "/manifests/")
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var m manifest
			if err := json.Unmarshal(body, &m); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			for _, d := range append([]descriptor{m.Config}, m.Layers...) {
				if !f.repoBlob[repo][d.Digest] {
					http.Error(w, fmt.Sprintf(`{"errors":[{"code":"MANIFEST_BLOB_UNKNOWN","detail":%q}]}`, d.Digest), http.StatusBadRequest)
					return
				}
			}
			dig := digestOf(body)
			f.mans[dig] = body
			if f.tags[repo] == nil {
				f.tags[repo] = map[string]string{}
			}
			f.tags[repo][ref] = dig
			w.Header().Set("Docker-Content-Digest", dig)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			dig := ref
			if !strings.HasPrefix(ref, "sha256:") {
				dig = f.tags[repo][ref]
			}
			body, ok := f.mans[dig]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Docker-Content-Digest", dig)
			_, _ = w.Write(body)
		case http.MethodDelete:
			found := false
			for t, d := range f.tags[repo] {
				if d == ref {
					delete(f.tags[repo], t)
					found = true
				}
			}
			if len(f.tags[repo]) == 0 {
				delete(f.tags, repo)
			}
			if !found {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		}
	default:
		f.t.Errorf("fake registry: unexpected %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

func (f *fakeRegistry) setDown(down bool) {
	f.mu.Lock()
	f.down = down
	f.mu.Unlock()
}

func (f *fakeRegistry) tagCount(repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tags[repo])
}
