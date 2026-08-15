package pkgcache

import (
	"net/http"
)

// ServeArtifactRoute handles a request under ArtifactRoute: decode the
// upstream URL the proxy itself advertised, check it against the SSRF fence,
// then stream it through the bounded cache.
//
// FAIL-OPEN, layer two: if the artifact cannot be fetched (upstream down,
// timeout, 5xx, cache unwritable) the client is 307-redirected to the real
// origin. npm, pip and pub all follow redirects, so a dead cache costs the
// job a slower, uncached download — never a failed one. A genuine upstream
// 404 is passed through as a 404, because "this version does not exist" is
// a real answer the job must see.
func (s *Server) ServeArtifactRoute(w http.ResponseWriter, r *http.Request, allow HostAllowlist, defaultContentType string) {
	upstream, err := ParseArtifactPath(r.URL.Path)
	if err != nil {
		s.cfg.Log.Warn("rejecting artifact path", "proxy", s.cfg.Name, "path", r.URL.Path, "error", err)
		http.Error(w, "bad artifact path", http.StatusBadRequest)
		return
	}
	// The encoded URL is client-controllable, so this check is what stops
	// the proxy being used as an open relay into the host's network (cloud
	// metadata endpoints, the LAN, other ephemerd control ports).
	if !allow.Allows(upstream) {
		s.cfg.Log.Warn("refusing artifact fetch from a non-allowlisted host",
			"proxy", s.cfg.Name, "url", upstream)
		http.Error(w, "artifact host is not allowed", http.StatusForbidden)
		return
	}

	err = s.fetch.ServeArtifact(w, r, Request{
		Key:                ArtifactKey(upstream),
		URL:                upstream,
		Immutable:          true,
		DefaultContentType: defaultContentType,
	})
	if err == nil {
		return
	}
	if IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	s.cfg.Log.Warn("artifact fetch failed; redirecting the job to the origin",
		"proxy", s.cfg.Name, "url", upstream, "error", err)
	http.Redirect(w, r, upstream, http.StatusTemporaryRedirect)
}

// RedirectUpstream is the metadata-path counterpart of the artifact
// fail-open: when a document cannot be fetched and nothing is cached, send
// the client to the origin rather than returning an error.
//
// The client then gets the ORIGINAL document, whose download URLs point at
// the origin CDN, so the whole job proceeds uncached and unbroken. That is
// strictly better than the alternative every "replace the registry" scheme
// suffers from (cargo's source.replace-with, notably), where a dead cache
// is a hard build failure.
func (s *Server) RedirectUpstream(w http.ResponseWriter, r *http.Request, upstreamURL string) {
	s.cfg.Log.Warn("document fetch failed; redirecting the job to the origin",
		"proxy", s.cfg.Name, "url", upstreamURL)
	http.Redirect(w, r, upstreamURL, http.StatusTemporaryRedirect)
}
