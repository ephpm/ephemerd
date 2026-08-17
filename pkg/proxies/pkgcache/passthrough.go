package pkgcache

import (
	"io"
	"net/http"
)

// hopByHopHeaders are never copied from an upstream response.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	// Not hop-by-hop, but deliberately dropped: a shared node-wide cache
	// must not hand one job a cookie set for another.
	"Set-Cookie",
}

// Passthrough relays a GET to upstream WITHOUT caching it, for endpoints
// that are neither package metadata nor an artifact (npm's search and ping,
// pub's advisory feed). Nothing is stored, so nothing can go stale.
//
// Fail-open: an unreachable upstream becomes a 307 to the origin rather than
// an error, so the client can complete the request itself.
func (s *Server) Passthrough(w http.ResponseWriter, r *http.Request, upstreamURL string) {
	req := Request{URL: upstreamURL, Accept: r.Header.Get("Accept")}
	resp, err := s.fetch.do(r.Context(), req, false, Meta{})
	if err != nil {
		s.RedirectUpstream(w, r, upstreamURL)
		return
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			s.cfg.Log.Debug("closing upstream body", "url", upstreamURL, "error", cerr)
		}
	}()

	out := w.Header()
	for k, vs := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		s.cfg.Log.Debug("relaying upstream response", "url", upstreamURL, "error", err)
	}
}

func isHopByHop(key string) bool {
	for _, h := range hopByHopHeaders {
		if http.CanonicalHeaderKey(key) == h {
			return true
		}
	}
	return false
}
