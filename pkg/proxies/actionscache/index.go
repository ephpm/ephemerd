package actionscache

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Manifest annotations carrying an entry's identity. The tag is only a hash,
// so these are the source of truth for key/version/ref.
const (
	annKey     = "dev.ephemerd.actions-cache.key"
	annVersion = "dev.ephemerd.actions-cache.version"
	annRef     = "dev.ephemerd.actions-cache.ref"
	annCreated = "dev.ephemerd.actions-cache.created"
	annSize    = "dev.ephemerd.actions-cache.size"
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations"`
}

// entry is one saved cache archive. Its bytes are Layers concatenated in
// order — one layer per uploaded block, so concurrent block uploads never
// have to be reassembled before they reach the registry.
type entry struct {
	Key     string
	Version string
	Ref     string
	Created time.Time
	Size    int64
	Layers  []descriptor

	repository string // registry repository (scopeRepository of Ref)
	tag        string
	digest     string // manifest digest, for deletion

	// lastUsed is when this node last served or saved the entry. In memory
	// only, so after a restart (or for entries another node wrote) eviction
	// falls back to Created.
	lastUsed time.Time
}

func (e *entry) recency() time.Time {
	if e.lastUsed.After(e.Created) {
		return e.lastUsed
	}
	return e.Created
}

func encodeManifest(e *entry, cfg descriptor) ([]byte, error) {
	m := manifest{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		Config:        cfg,
		Layers:        e.Layers,
		Annotations: map[string]string{
			annKey:     e.Key,
			annVersion: e.Version,
			annRef:     e.Ref,
			annCreated: e.Created.UTC().Format(time.RFC3339Nano),
			annSize:    strconv.FormatInt(e.Size, 10),
		},
	}
	if m.Layers == nil {
		m.Layers = []descriptor{}
	}
	return json.Marshal(m)
}

func decodeManifest(body []byte) (*entry, error) {
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m.Config.MediaType != configMediaType {
		return nil, fmt.Errorf("not an actions-cache manifest (config %q)", m.Config.MediaType)
	}
	e := &entry{
		Key:     m.Annotations[annKey],
		Version: m.Annotations[annVersion],
		Ref:     m.Annotations[annRef],
		Layers:  m.Layers,
	}
	e.Created, _ = time.Parse(time.RFC3339Nano, m.Annotations[annCreated])
	for _, l := range m.Layers {
		e.Size += l.Size
	}
	if want, err := strconv.ParseInt(m.Annotations[annSize], 10, 64); err == nil && want != e.Size {
		return nil, fmt.Errorf("layers sum to %d bytes, annotation says %d", e.Size, want)
	}
	return e, nil
}

// configBlob is the small config object every manifest points at. registry:2
// insists the config blob exists; this one carries the same identity as the
// annotations, which also makes every entry's config digest distinct.
func configBlob(e *entry) []byte {
	b, _ := json.Marshal(map[string]string{
		"key":     e.Key,
		"version": e.Version,
		"ref":     e.Ref,
		"created": e.Created.UTC().Format(time.RFC3339Nano),
	})
	return b
}

// scopeIndex is the in-memory view of one registry repository (one ref of
// one forge repo): tag -> entry. It is refreshed from the registry's tag list
// on every lookup, because other nodes write to the same registry; only tags
// it has not seen cost a manifest fetch.
type scopeIndex struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// index is every scopeIndex this node has touched, plus the bookkeeping for
// per-repo eviction.
type index struct {
	reg *registry

	mu     sync.Mutex
	scopes map[string]*scopeIndex // registry repository -> index
	// lastSweep is when a repo namespace's full catalog was last walked for
	// eviction; the walk is rate-limited because the catalog is global.
	lastSweep map[string]time.Time
}

func newIndex(reg *registry) *index {
	return &index{reg: reg, scopes: map[string]*scopeIndex{}, lastSweep: map[string]time.Time{}}
}

func (ix *index) scope(repository string) *scopeIndex {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	s, ok := ix.scopes[repository]
	if !ok {
		s = &scopeIndex{entries: map[string]*entry{}}
		ix.scopes[repository] = s
	}
	return s
}

// refresh syncs one repository's index with the registry and returns a
// snapshot of its entries.
func (ix *index) refresh(ctx context.Context, repository string) ([]*entry, error) {
	tags, err := ix.reg.listTags(ctx, repository)
	if err != nil {
		return nil, err
	}
	s := ix.scope(repository)
	s.mu.Lock()
	known := make(map[string]*entry, len(s.entries))
	for t, e := range s.entries {
		known[t] = e
	}
	s.mu.Unlock()

	live := make(map[string]*entry, len(tags))
	for _, t := range tags {
		if e, ok := known[t]; ok {
			live[t] = e
			continue
		}
		body, digest, err := ix.reg.getManifest(ctx, repository, t)
		if err != nil {
			continue // deleted between list and fetch, or not ours
		}
		e, err := decodeManifest(body)
		if err != nil {
			continue
		}
		e.repository, e.tag, e.digest = repository, t, digest
		live[t] = e
	}

	s.mu.Lock()
	s.entries = live
	out := make([]*entry, 0, len(live))
	for _, e := range live {
		out = append(out, e)
	}
	s.mu.Unlock()
	return out, nil
}

func (ix *index) add(e *entry) {
	s := ix.scope(e.repository)
	s.mu.Lock()
	s.entries[e.tag] = e
	s.mu.Unlock()
}

func (ix *index) remove(e *entry) {
	s := ix.scope(e.repository)
	s.mu.Lock()
	if cur, ok := s.entries[e.tag]; ok && cur.digest == e.digest {
		delete(s.entries, e.tag)
	}
	s.mu.Unlock()
}

// lookup finds the entry a restore should get, with GitHub's matching rules:
// for each readable ref in order, an exact key match first, then each restore
// key as a prefix in order, newest entry winning a prefix match. An entry
// only ever matches its own version — the same key with other paths or
// compression is a different cache.
func (ix *index) lookup(ctx context.Context, s Scope, key string, restoreKeys []string, version string) (*entry, error) {
	var firstErr error
	for _, ref := range s.Reads {
		entries, err := ix.refresh(ctx, scopeRepository(s.Repo, ref))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if e := match(entries, key, restoreKeys, version); e != nil {
			return e, nil
		}
	}
	return nil, firstErr
}

func match(entries []*entry, key string, restoreKeys []string, version string) *entry {
	for _, e := range entries {
		if e.Version == version && e.Key == key {
			return e
		}
	}
	for _, rk := range restoreKeys {
		if rk == "" {
			continue
		}
		var best *entry
		for _, e := range entries {
			if e.Version != version || !strings.HasPrefix(e.Key, rk) {
				continue
			}
			if best == nil || e.Created.After(best.Created) {
				best = e
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

// exists reports whether (key, version) is already saved under ref. Entries
// are immutable, as on GitHub: a second save of the same key is refused.
func (ix *index) exists(ctx context.Context, repo, ref, key, version string) (bool, error) {
	entries, err := ix.refresh(ctx, scopeRepository(repo, ref))
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Key == key && e.Version == version {
			return true, nil
		}
	}
	return false, nil
}

// evict deletes the least recently used entries of repo until its total size
// is within maxBytes. The full namespace walk (every ref the repo ever saved
// under) is rate-limited to once per sweepEvery; between walks only the
// scopes this node has touched are counted, which can only under-count and
// so only ever delays an eviction — it never deletes too much.
func (ix *index) evict(ctx context.Context, repo string, maxBytes int64, sweepEvery time.Duration, now time.Time) ([]*entry, error) {
	if maxBytes <= 0 {
		return nil, nil
	}
	ns := repoNamespace(repo) + "/"

	ix.mu.Lock()
	walk := now.Sub(ix.lastSweep[ns]) >= sweepEvery
	if walk {
		ix.lastSweep[ns] = now
	}
	var repos []string
	for r := range ix.scopes {
		if strings.HasPrefix(r, ns) {
			repos = append(repos, r)
		}
	}
	ix.mu.Unlock()

	if walk {
		all, err := ix.reg.catalog(ctx, ns)
		if err != nil {
			return nil, err
		}
		repos = all
	}

	var entries []*entry
	var total int64
	for _, r := range repos {
		es, err := ix.refresh(ctx, r)
		if err != nil {
			continue
		}
		for _, e := range es {
			entries = append(entries, e)
			total += e.Size
		}
	}
	if total <= maxBytes {
		return nil, nil
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].recency().Before(entries[j].recency()) })
	var evicted []*entry
	for _, e := range entries {
		if total <= maxBytes {
			break
		}
		if err := ix.reg.deleteManifest(ctx, e.repository, e.digest); err != nil {
			return evicted, err
		}
		ix.remove(e)
		total -= e.Size
		evicted = append(evicted, e)
	}
	return evicted, nil
}
