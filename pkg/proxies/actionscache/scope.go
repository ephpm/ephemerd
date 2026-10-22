package actionscache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Scope is what one job may do with the cache. It is decided by ephemerd
// from the forge's own record of the job (see the scheduler's in_progress
// binding), never from anything the job sends.
type Scope struct {
	// Repo is the forge-native "owner/repo" the job belongs to. Entries of
	// different repos never meet: each repo gets its own registry namespace.
	Repo string
	// Write is the ref this job saves entries under, e.g. "refs/heads/main"
	// or "refs/pull/12/merge". Empty means read-only.
	Write string
	// Reads are the refs a restore searches, in order — the job's own ref
	// first, then the default branch. This mirrors GitHub: a PR can restore
	// what main saved, but nothing a PR saves is ever visible to main.
	Reads []string
}

func (s Scope) canRead(ref string) bool {
	for _, r := range s.Reads {
		if r == ref {
			return true
		}
	}
	return false
}

// repoNamespace is the registry repository prefix one forge repo's entries
// live under: actions-cache/<owner>/<repo>-<hash>.
//
// Registry path components are a narrower alphabet than repo names (no
// uppercase, no "."/"_" next to another separator), so both parts are
// squashed to [a-z0-9-]. Squashing alone could map two repos onto one
// namespace ("foo.bar" and "foo-bar") and let one read the other's entries;
// the hash of the exact, case-folded name keeps them apart.
func repoNamespace(repo string) string {
	repo = strings.ToLower(strings.TrimSpace(repo))
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		owner, name = "_", repo
	}
	sum := sha256.Sum256([]byte(repo))
	return "actions-cache/" + pathComponent(owner) + "/" + pathComponent(name) + "-" + hex.EncodeToString(sum[:4])
}

// scopeRepository is the registry repository holding the entries one ref
// saved. The ref itself (slashes and all) is hashed into a valid component
// and kept verbatim in each entry's annotations.
func scopeRepository(repo, ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return repoNamespace(repo) + "/s" + hex.EncodeToString(sum[:8])
}

// entryTag is the tag one (key, version) pair is stored under. Cache keys
// are up to 512 arbitrary characters and tags at most 128 from a small
// alphabet, so the tag is a digest; the real key travels in the manifest.
// Version is part of it because the same key saved with different paths or
// compression is a different entry (that is what version encodes).
func entryTag(key, version string) string {
	sum := sha256.Sum256([]byte(version + "\x00" + key))
	return "k" + hex.EncodeToString(sum[:20])
}

// pathComponent squashes s into a valid registry path component: lowercase
// alphanumerics joined by single hyphens.
func pathComponent(s string) string {
	var b strings.Builder
	sep := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if sep && b.Len() > 0 {
				b.WriteByte('-')
			}
			sep = false
			b.WriteRune(r)
		default:
			sep = true
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
