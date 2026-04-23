//go:build !linux

package dind

import "net/http"

// handleImageBuild is not supported on non-Linux platforms.
// Buildah requires Linux namespaces and overlay filesystems.
func (s *Server) handleImageBuild(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"message": "docker build is only supported on Linux",
	})
}
