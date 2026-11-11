package runnerversion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/runner"
)

// Store is the content-addressed cache of verified runner archives.
//
// Layout under <dataDir>/runners/verified:
//
//	blobs/<sha256>.<ext>      the archive, named by its hash
//	blobs/<sha256>.json       sidecar (Sidecar)
//	extracted/<version>-<goos>/   extracted tree, ".extracted" marker last
//	current                   Pointer: what the selector should serve
//	previous                  the Pointer that current replaced
//	applied-manifest          RFC3339 published_at of the last applied manifest
//
// Invariants: nothing is extracted before its hash verified over the whole
// file (Put); the cache holds at most current+previous (Prune); pointer
// updates are an atomic rename (Swap).
type Store struct {
	root string
	goos string
	log  *slog.Logger

	// mu serializes Put/Extract/Swap/Prune so two upgrades cannot
	// interleave. Readers (Current, Previous) take it too; they are rare.
	mu sync.Mutex
}

// Pointer is the content of the current/previous files.
type Pointer struct {
	SHA256    string    `json:"sha256"`
	Version   string    `json:"version"`
	Ext       string    `json:"ext"`
	Dir       string    `json:"dir"`
	AppliedAt time.Time `json:"applied_at"`
}

// Sidecar records where a blob came from and what verified it.
type Sidecar struct {
	Version   string    `json:"version"`
	Platform  string    `json:"platform"`
	URL       string    `json:"url"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	FetchedAt time.Time `json:"fetched_at"`
	// Sources names every party that vouched for SHA256 ("manifest",
	// "github-runners-downloads", ...). Informational.
	Sources []string `json:"sources"`
}

var (
	// ErrChecksumMismatch means the bytes did not hash to what was promised.
	ErrChecksumMismatch = errors.New("runner archive checksum mismatch")
	// ErrTooLarge means the stream exceeded the configured cap.
	ErrTooLarge = errors.New("runner archive exceeds size cap")
	// ErrNotInStore means the blob or its extraction is missing.
	ErrNotInStore = errors.New("runner archive not in store")
)

const (
	tmpPrefix       = ".tmp-"
	extractedMarker = ".extracted"
)

// extractedNameRe matches the directory names extractedDir produces
// ("2.338.2-linux"); Prune deletes nothing that does not match.
var extractedNameRe = regexp.MustCompile(`^\d+\.\d+\.\d+-[a-z0-9]+$`)

// NewStore creates a store rooted at <dataDir>/runners/verified. Init must
// be called before use.
func NewStore(dataDir string, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{
		root: filepath.Join(dataDir, "runners", "verified"),
		goos: goruntime.GOOS,
		log:  log,
	}
}

// Root is the store directory.
func (s *Store) Root() string { return s.root }

func (s *Store) blobsDir() string                { return filepath.Join(s.root, "blobs") }
func (s *Store) extractedRoot() string           { return filepath.Join(s.root, "extracted") }
func (s *Store) blobPath(sha, ext string) string { return filepath.Join(s.blobsDir(), sha+"."+ext) }
func (s *Store) sidecarPath(sha string) string   { return filepath.Join(s.blobsDir(), sha+".json") }
func (s *Store) extractedDir(version string) string {
	return filepath.Join(s.extractedRoot(), version+"-"+s.goos)
}
func (s *Store) pointerPath(name string) string { return filepath.Join(s.root, name) }

// Init creates the directories and removes temp files left by a crash.
func (s *Store) Init() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range []string{s.blobsDir(), s.extractedRoot()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
	}
	entries, err := os.ReadDir(s.blobsDir())
	if err != nil {
		return fmt.Errorf("reading blobs: %w", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			p := filepath.Join(s.blobsDir(), e.Name())
			if err := os.Remove(p); err != nil {
				s.log.Warn("removing stale temp file", "path", p, "error", err)
			}
		}
	}
	return nil
}

// Has reports whether a verified blob with this hash is present.
func (s *Store) Has(sha, ext string) bool {
	_, err := os.Stat(s.blobPath(sha, ext))
	return err == nil
}

// Put streams r into the store, hashing as it goes, and commits the blob
// ONLY if the hash equals meta.SHA256 and the size stays under maxBytes.
// On any failure the temp file is removed and nothing is committed.
// Returns the blob path.
func (s *Store) Put(r io.Reader, meta Sidecar, maxBytes int64) (string, error) {
	if !sha256Re.MatchString(meta.SHA256) {
		return "", fmt.Errorf("%w: expected sha256 is not 64 hex chars", ErrChecksumMismatch)
	}
	ext := ArchiveExt(meta.Platform)
	s.mu.Lock()
	defer s.mu.Unlock()

	final := s.blobPath(meta.SHA256, ext)
	if _, err := os.Stat(final); err == nil {
		s.log.Info("runner archive already in store", "sha256", meta.SHA256)
		return final, nil
	}

	tmp, err := os.CreateTemp(s.blobsDir(), tmpPrefix+"*."+ext)
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	discard := func() {
		if err := tmp.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			s.log.Warn("closing temp file", "path", tmpName, "error", err)
		}
		if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("removing temp file", "path", tmpName, "error", err)
		}
	}

	h := sha256.New()
	limited := io.LimitReader(r, maxBytes+1)
	n, err := io.Copy(io.MultiWriter(tmp, h), limited)
	if err != nil {
		discard()
		return "", fmt.Errorf("writing archive: %w", err)
	}
	if n > maxBytes {
		discard()
		return "", fmt.Errorf("%w: more than %d bytes", ErrTooLarge, maxBytes)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != meta.SHA256 {
		discard()
		return "", fmt.Errorf("%w: got %s want %s", ErrChecksumMismatch, got, meta.SHA256)
	}
	if err := tmp.Sync(); err != nil {
		discard()
		return "", fmt.Errorf("syncing archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		discard()
		return "", fmt.Errorf("closing archive: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		discard()
		return "", fmt.Errorf("committing archive: %w", err)
	}
	meta.Size = n
	if meta.FetchedAt.IsZero() {
		meta.FetchedAt = time.Now()
	}
	if err := writeJSONAtomic(s.sidecarPath(meta.SHA256), meta); err != nil {
		if rmErr := os.Remove(final); rmErr != nil {
			s.log.Warn("removing blob after sidecar failure", "path", final, "error", rmErr)
		}
		return "", fmt.Errorf("writing sidecar: %w", err)
	}
	s.log.Info("runner archive committed", "sha256", meta.SHA256, "version", meta.Version, "bytes", n)
	return final, nil
}

// Sidecar reads a blob's sidecar.
func (s *Store) Sidecar(sha string) (Sidecar, error) {
	var sc Sidecar
	data, err := os.ReadFile(s.sidecarPath(sha))
	if err != nil {
		return sc, fmt.Errorf("%w: %w", ErrNotInStore, err)
	}
	if err := json.Unmarshal(data, &sc); err != nil {
		return sc, fmt.Errorf("sidecar %s: %w", sha, err)
	}
	return sc, nil
}

// Extract unpacks a committed blob into extracted/<version>-<goos>. It is
// a no-op when the marker already exists. The blob must have been
// committed through Put (hash verified); Extract re-checks the sidecar
// exists but does not re-hash — that is Verify's job at boot.
func (s *Store) Extract(sha, version string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.extractLocked(sha, version)
}

func (s *Store) extractLocked(sha, version string) (string, error) {
	sc, err := s.Sidecar(sha)
	if err != nil {
		return "", err
	}
	blob := s.blobPath(sha, ArchiveExt(sc.Platform))
	if _, err := os.Stat(blob); err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotInStore, err)
	}
	dir := s.extractedDir(version)
	marker := filepath.Join(dir, extractedMarker)
	if _, err := os.Stat(marker); err == nil {
		return dir, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("cleaning partial extraction: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	f, err := os.Open(blob)
	if err != nil {
		return "", fmt.Errorf("opening blob: %w", err)
	}
	extractErr := runner.ExtractArchive(f, blob, dir)
	if err := f.Close(); err != nil {
		s.log.Warn("closing blob", "path", blob, "error", err)
	}
	if extractErr != nil {
		if err := os.RemoveAll(dir); err != nil {
			s.log.Warn("removing failed extraction", "dir", dir, "error", err)
		}
		return "", fmt.Errorf("extracting runner: %w", extractErr)
	}
	if err := os.WriteFile(marker, []byte(sha), 0o644); err != nil {
		return "", fmt.Errorf("writing extraction marker: %w", err)
	}
	s.log.Info("runner archive extracted", "version", version, "dir", dir)
	return dir, nil
}

// Swap makes <sha,version> current. The blob must be committed and
// extracted. The pointer is replaced atomically; the old pointer becomes
// previous. Swap never deletes anything — call Prune afterwards.
func (s *Store) Swap(sha, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, err := s.Sidecar(sha)
	if err != nil {
		return err
	}
	ext := ArchiveExt(sc.Platform)
	if !s.Has(sha, ext) {
		return fmt.Errorf("%w: blob %s", ErrNotInStore, sha)
	}
	dir := s.extractedDir(version)
	if _, err := os.Stat(filepath.Join(dir, extractedMarker)); err != nil {
		return fmt.Errorf("%w: %s not extracted", ErrNotInStore, version)
	}
	old, hadOld, err := s.readPointer("current")
	if err != nil {
		return err
	}
	if hadOld && old.SHA256 == sha {
		return nil
	}
	next := Pointer{SHA256: sha, Version: version, Ext: ext, Dir: dir, AppliedAt: time.Now()}
	if err := writeJSONAtomic(s.pointerPath("current"), next); err != nil {
		return fmt.Errorf("writing current pointer: %w", err)
	}
	if hadOld {
		if err := writeJSONAtomic(s.pointerPath("previous"), old); err != nil {
			// current is already valid; previous is a convenience.
			s.log.Warn("writing previous pointer", "error", err)
		}
	}
	s.log.Warn("runner swapped", "version", version, "sha256", sha, "dir", dir, "previous", old.Version)
	return nil
}

// Rollback makes previous current (and current previous).
func (s *Store) Rollback() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok, err := s.readPointer("previous")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no previous runner to roll back to", ErrNotInStore)
	}
	cur, hadCur, err := s.readPointer("current")
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(prev.Dir, extractedMarker)); err != nil {
		return fmt.Errorf("%w: previous %s not extracted", ErrNotInStore, prev.Version)
	}
	prev.AppliedAt = time.Now()
	if err := writeJSONAtomic(s.pointerPath("current"), prev); err != nil {
		return fmt.Errorf("writing current pointer: %w", err)
	}
	if hadCur {
		if err := writeJSONAtomic(s.pointerPath("previous"), cur); err != nil {
			s.log.Warn("writing previous pointer", "error", err)
		}
	}
	s.log.Warn("runner rolled back", "version", prev.Version, "sha256", prev.SHA256)
	return nil
}

// Current returns the current pointer, if any.
func (s *Store) Current() (Pointer, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readPointer("current")
}

// Previous returns the previous pointer, if any.
func (s *Store) Previous() (Pointer, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readPointer("previous")
}

func (s *Store) readPointer(name string) (Pointer, bool, error) {
	var p Pointer
	data, err := os.ReadFile(s.pointerPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("reading %s pointer: %w", name, err)
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, false, fmt.Errorf("%s pointer: %w", name, err)
	}
	return p, true, nil
}

// VerifyResult is one pointer's boot-time verification.
type VerifyResult struct {
	Pointer Pointer
	OK      bool
	Err     error
}

// Verify re-hashes the blobs behind current and previous. A blob whose
// hash no longer matches its name is deleted along with its extraction and
// its pointer, so the selector can never pick it. Returns one result per
// pointer that existed.
func (s *Store) Verify() []VerifyResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []VerifyResult
	for _, name := range []string{"current", "previous"} {
		p, ok, err := s.readPointer(name)
		if err != nil {
			out = append(out, VerifyResult{Err: err})
			if rmErr := os.Remove(s.pointerPath(name)); rmErr != nil {
				s.log.Warn("removing unreadable pointer", "name", name, "error", rmErr)
			}
			continue
		}
		if !ok {
			continue
		}
		res := VerifyResult{Pointer: p}
		got, err := hashFile(s.blobPath(p.SHA256, p.Ext))
		switch {
		case err != nil:
			res.Err = fmt.Errorf("%w: %w", ErrNotInStore, err)
		case got != p.SHA256:
			res.Err = fmt.Errorf("%w: blob %s hashes to %s", ErrChecksumMismatch, p.SHA256, got)
		case !fileExists(filepath.Join(p.Dir, extractedMarker)):
			// Re-extractable from a good blob; do that rather than drop it.
			if _, exErr := s.extractLocked(p.SHA256, p.Version); exErr != nil {
				res.Err = exErr
			} else {
				res.OK = true
			}
		default:
			res.OK = true
		}
		if !res.OK {
			s.log.Error("verified-cache runner failed boot verification; dropping it",
				"pointer", name, "version", p.Version, "sha256", p.SHA256, "error", res.Err)
			s.dropLocked(p, name)
		}
		out = append(out, res)
	}
	return out
}

func (s *Store) dropLocked(p Pointer, pointerName string) {
	for _, path := range []string{s.blobPath(p.SHA256, p.Ext), s.sidecarPath(p.SHA256), s.pointerPath(pointerName)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("removing", "path", path, "error", err)
		}
	}
	if err := os.RemoveAll(p.Dir); err != nil {
		s.log.Warn("removing extraction", "dir", p.Dir, "error", err)
	}
}

// Prune deletes every blob, sidecar and extracted directory not referenced
// by current or previous. The cache is bounded to those two; the fleet has
// died from full disks before.
func (s *Store) Prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keepSHA := map[string]bool{}
	keepDir := map[string]bool{}
	for _, name := range []string{"current", "previous"} {
		p, ok, err := s.readPointer(name)
		if err != nil {
			return err
		}
		if ok {
			keepSHA[p.SHA256] = true
			keepDir[filepath.Clean(p.Dir)] = true
		}
	}
	entries, err := os.ReadDir(s.blobsDir())
	if err != nil {
		return fmt.Errorf("reading blobs: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		sha := strings.SplitN(name, ".", 2)[0]
		if keepSHA[sha] {
			continue
		}
		p := filepath.Join(s.blobsDir(), name)
		if err := os.Remove(p); err != nil {
			s.log.Warn("pruning blob", "path", p, "error", err)
			continue
		}
		s.log.Info("pruned unreferenced runner archive", "path", p)
	}
	dirs, err := os.ReadDir(s.extractedRoot())
	if err != nil {
		return fmt.Errorf("reading extracted: %w", err)
	}
	for _, d := range dirs {
		p := filepath.Join(s.extractedRoot(), d.Name())
		if keepDir[filepath.Clean(p)] {
			continue
		}
		// Only this store's own "<version>-<goos>" extractions are ours to
		// delete. Anything else under the root (a per-job tree created in
		// the wrong place, an operator's file) is reported, never removed.
		if !extractedNameRe.MatchString(d.Name()) {
			s.log.Warn("foreign entry under the runner store's extracted root; leaving it", "path", p)
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			s.log.Warn("pruning extraction", "dir", p, "error", err)
			continue
		}
		s.log.Info("pruned unreferenced runner extraction", "dir", p)
	}
	return nil
}

// LastApplied is the published_at of the last manifest that caused a
// swap; manifests at or before it are replays and ignored.
func (s *Store) LastApplied() (time.Time, error) {
	data, err := os.ReadFile(s.pointerPath("applied-manifest"))
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
}

// SetLastApplied records a manifest's published_at.
func (s *Store) SetLastApplied(t time.Time) error {
	return writeFileAtomic(s.pointerPath("applied-manifest"), []byte(t.UTC().Format(time.RFC3339)+"\n"))
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// writeFileAtomic writes to a temp file in the same directory and renames
// it over path. os.Rename replaces an existing file on every OS Go
// supports, including Windows (MOVEFILE_REPLACE_EXISTING).
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tmpPrefix+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	// On any failure every cleanup error is joined onto the cause, so
	// nothing is silently discarded.
	fail := func(cause error, closed bool) error {
		errs := []error{cause}
		if !closed {
			if cerr := tmp.Close(); cerr != nil {
				errs = append(errs, cerr)
			}
		}
		if rerr := os.Remove(name); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			errs = append(errs, rerr)
		}
		return errors.Join(errs...)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err, false)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err, false)
	}
	if err := tmp.Close(); err != nil {
		return fail(err, true)
	}
	if err := os.Rename(name, path); err != nil {
		return fail(err, true)
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	if err := f.Close(); err != nil && copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		return "", copyErr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashReader hashes a stream (used to verify the embedded archive at boot).
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
