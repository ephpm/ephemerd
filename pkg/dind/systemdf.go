package dind

import (
	"net/http"
	"sort"
)

// GET /system/df — `docker system df`.
//
// Ask (3) of #149, and the smallest of the three by a wide margin, but it cost
// real time during that incident: the maintenance job dispatched to work out
// how much of the node's disk was build cache failed on an unimplemented
// endpoint, so the operator had to go to the host anyway. A diagnostic that
// only works when you already have the access it was meant to save you is
// worse than no diagnostic, because you spend a job round-trip finding out.
//
// What this can honestly report is the per-job view: the images this job's
// socket knows about, and the containers and networks it created. It cannot
// report the shared host-side stores — the BuildKit build cache lives outside
// any job's namespace and is deliberately unreachable from inside one (#137).
// Rather than omit the field and let `docker system df` print a confusing
// zero, BuildCache is reported as an explicit empty set, and the endpoint
// answers with fields Docker's client can parse in both its table and its
// verbose mode.

// diskUsageResponse mirrors the subset of Docker's GET /system/df response
// that the CLI needs to render. Field names and shapes follow the Docker
// Engine API (types.DiskUsage); the CLI unmarshals into its own struct and
// ignores anything it does not know, but it does require these to be present
// and correctly typed.
type diskUsageResponse struct {
	LayersSize  int64            `json:"LayersSize"`
	Images      []map[string]any `json:"Images"`
	Containers  []map[string]any `json:"Containers"`
	Volumes     []map[string]any `json:"Volumes"`
	BuildCache  []map[string]any `json:"BuildCache"`
	BuilderSize int64            `json:"BuilderSize"`
}

func (s *Server) handleSystemDF(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	resp := diskUsageResponse{
		// Non-nil empties: Docker's CLI ranges over these, and a JSON null
		// makes `docker system df -v` panic on some client versions.
		Images:     make([]map[string]any, 0, len(s.images)),
		Containers: make([]map[string]any, 0, len(s.containers)),
		Volumes:    []map[string]any{},
		BuildCache: []map[string]any{},
	}

	refs := make([]string, 0, len(s.images))
	for ref := range s.images {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	for _, ref := range refs {
		img := s.images[ref]
		resp.LayersSize += img.Size
		resp.Images = append(resp.Images, map[string]any{
			"Id":          img.ID,
			"RepoTags":    []string{ref},
			"RepoDigests": []string{},
			"Size":        img.Size,
			// SharedSize -1 is Docker's "not computed" sentinel. Layers in
			// the job namespace are shared with the per-repo cache
			// namespace in ways this server does not track, and guessing
			// would make the numbers add up to something untrue.
			"SharedSize":  int64(-1),
			"VirtualSize": img.Size,
			"Containers":  s.containersUsingLocked(ref),
		})
	}

	ids := make([]string, 0, len(s.containers))
	for id := range s.containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		c := s.containers[id]
		resp.Containers = append(resp.Containers, map[string]any{
			"Id":         id,
			"Names":      []string{"/" + c.Name},
			"Image":      c.Image,
			"SizeRw":     int64(0),
			"SizeRootFs": int64(0),
			"State":      c.Status,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// containersUsingLocked counts the containers created through this socket
// that run the given image ref. Caller holds s.mu.
func (s *Server) containersUsingLocked(ref string) int64 {
	var n int64
	for _, c := range s.containers {
		if c.Image == ref {
			n++
		}
	}
	return n
}
