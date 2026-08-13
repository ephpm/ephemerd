package dind

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The endpoint exists because a maintenance job dispatched to measure the
// node's disk during #149 failed on an unimplemented route, so the operator
// had to go to the host anyway.
func TestSystemDFIsImplemented(t *testing.T) {
	s := &Server{
		log:        slog.Default(),
		images:     map[string]*imageEntry{"alpine:3.20": {ID: "sha256:aaa", Size: 7_000_000}},
		containers: map[string]*containerEntry{},
	}

	for _, path := range []string{"/system/df", "/v1.45/system/df"} {
		rec := httptest.NewRecorder()
		s.route(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %q)", path, rec.Code, rec.Body.String())
		}
		var got diskUsageResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: decoding response: %v", path, err)
		}
		if len(got.Images) != 1 {
			t.Fatalf("%s: got %d images, want 1", path, len(got.Images))
		}
		if got.LayersSize != 7_000_000 {
			t.Errorf("%s: LayersSize = %d, want 7000000", path, got.LayersSize)
		}
	}
}

func TestSystemDFEmptyCollectionsAreNotNull(t *testing.T) {
	// Docker's CLI ranges over every one of these. A JSON null turns
	// `docker system df -v` — a diagnostic — into a crash.
	s := &Server{
		log:        slog.Default(),
		images:     map[string]*imageEntry{},
		containers: map[string]*containerEntry{},
	}
	rec := httptest.NewRecorder()
	s.route(rec, httptest.NewRequest(http.MethodGet, "/system/df", nil))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	for _, field := range []string{"Images", "Containers", "Volumes", "BuildCache"} {
		v, ok := raw[field]
		if !ok {
			t.Errorf("%s missing from the response", field)
			continue
		}
		if string(v) != "[]" {
			t.Errorf("%s = %s, want []", field, v)
		}
	}
}

func TestSystemDFCountsContainersPerImage(t *testing.T) {
	s := &Server{
		log: slog.Default(),
		images: map[string]*imageEntry{
			"alpine:3.20": {ID: "sha256:aaa", Size: 7_000_000},
			"debian:trix": {ID: "sha256:bbb", Size: 120_000_000},
		},
		containers: map[string]*containerEntry{
			"c1": {ID: "c1", Name: "one", Image: "alpine:3.20", Status: "running"},
			"c2": {ID: "c2", Name: "two", Image: "alpine:3.20", Status: "exited"},
		},
	}
	rec := httptest.NewRecorder()
	s.route(rec, httptest.NewRequest(http.MethodGet, "/system/df", nil))

	var got diskUsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.LayersSize != 127_000_000 {
		t.Errorf("LayersSize = %d, want 127000000", got.LayersSize)
	}
	if len(got.Containers) != 2 {
		t.Fatalf("got %d containers, want 2", len(got.Containers))
	}

	// Images are emitted in sorted ref order, so alpine comes first.
	if n := got.Images[0]["Containers"]; n != float64(2) {
		t.Errorf("alpine container count = %v, want 2", n)
	}
	if n := got.Images[1]["Containers"]; n != float64(0) {
		t.Errorf("debian container count = %v, want 0", n)
	}
}

func TestSystemDFRejectsNonGET(t *testing.T) {
	s := &Server{log: slog.Default(), images: map[string]*imageEntry{}, containers: map[string]*containerEntry{}}
	rec := httptest.NewRecorder()
	s.route(rec, httptest.NewRequest(http.MethodPost, "/system/df", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("POST /system/df returned 200; it should fall through to not-implemented")
	}
}
