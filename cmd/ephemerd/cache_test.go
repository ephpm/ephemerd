package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// mkFile creates a file of the given size with parent dirs.
func mkFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDirSize_SumsRegularFiles(t *testing.T) {
	dir := t.TempDir()
	mkFile(t, filepath.Join(dir, "a"), 100)
	mkFile(t, filepath.Join(dir, "sub", "b"), 250)
	mkFile(t, filepath.Join(dir, "sub", "deep", "c"), 5)

	got, err := dirSize(dir)
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}
	if got != 355 {
		t.Errorf("dirSize = %d, want 355", got)
	}
}

func TestDirSize_MissingPathIsZero(t *testing.T) {
	got, err := dirSize(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("dirSize on missing path should not error, got: %v", err)
	}
	if got != 0 {
		t.Errorf("dirSize = %d, want 0 for missing path", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{5 * 1024 * 1024 * 1024, "5.0 GB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestIsUnder(t *testing.T) {
	root := filepath.Join(string(os.PathSeparator)+"var", "lib", "ephemerd")
	cases := []struct {
		target string
		want   bool
	}{
		{root, true},
		{filepath.Join(root, "images"), true},
		{filepath.Join(root, "cache", "gomod"), true},
		{filepath.Join(root, ".."), false},
		{filepath.Join(root, "..", "..", "etc"), false},
		// Shared-prefix sibling must NOT count as under root.
		{root + "2", false},
	}
	for _, c := range cases {
		if got := isUnder(c.target, root); got != c.want {
			t.Errorf("isUnder(%q, %q) = %v, want %v", c.target, root, got, c.want)
		}
	}
}

func TestResolveCacheRoot_StaysUnderDataDir(t *testing.T) {
	data := t.TempDir()
	for _, c := range managedCaches() {
		root, err := resolveCacheRoot(data, c)
		if err != nil {
			t.Fatalf("resolveCacheRoot(%q): %v", c.Name, err)
		}
		if !isUnder(root, mustAbs(t, data)) {
			t.Errorf("cache %q root %q not under data dir %q", c.Name, root, data)
		}
	}
}

// TestResolveCacheRoot_RejectsTraversal is the core safety guard test: a
// cache whose Rel escapes the data dir must be refused, never resolved.
func TestResolveCacheRoot_RejectsTraversal(t *testing.T) {
	data := t.TempDir()
	evil := cacheEntry{Name: "evil", Rel: filepath.Join("..", "..", "etc")}
	if _, err := resolveCacheRoot(data, evil); err == nil {
		t.Fatal("expected traversal cache Rel to be rejected, got nil error")
	}
}

func TestResolveCacheRoot_EmptyDataDirErrors(t *testing.T) {
	if _, err := resolveCacheRoot("", cacheEntry{Name: "images", Rel: "images"}); err == nil {
		t.Fatal("expected error for empty data dir")
	}
}

func TestClearCacheDir_RemovesContentsKeepsRoot(t *testing.T) {
	data := t.TempDir()
	c, _ := cacheByName("images")
	root, err := resolveCacheRoot(data, c)
	if err != nil {
		t.Fatal(err)
	}
	mkFile(t, filepath.Join(root, "a.tar"), 1000)
	mkFile(t, filepath.Join(root, "b.tar"), 2000)

	freed, err := clearCacheDir(data, root, c.KeepDirs)
	if err != nil {
		t.Fatalf("clearCacheDir: %v", err)
	}
	if freed != 3000 {
		t.Errorf("freed = %d, want 3000", freed)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root after clear: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty cache root, got %d entries", len(entries))
	}
}

func TestClearCacheDir_PreservesKeepDirs(t *testing.T) {
	data := t.TempDir()
	c, _ := cacheByName("vm") // KeepDirs = ["embed"]
	root, err := resolveCacheRoot(data, c)
	if err != nil {
		t.Fatal(err)
	}
	mkFile(t, filepath.Join(root, "embed", "asset.bin"), 42)
	mkFile(t, filepath.Join(root, "macos", "base.img"), 9999)
	mkFile(t, filepath.Join(root, "run", "distro", "ephemerd-linux"), 100)

	if _, err := clearCacheDir(data, root, c.KeepDirs); err != nil {
		t.Fatalf("clearCacheDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "embed", "asset.bin")); err != nil {
		t.Errorf("embed asset should have been preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "macos")); !os.IsNotExist(err) {
		t.Errorf("macos dir should have been removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "run")); !os.IsNotExist(err) {
		t.Errorf("run dir should have been removed, stat err = %v", err)
	}
}

func TestClearCacheDir_MissingRootIsNoop(t *testing.T) {
	data := t.TempDir()
	c, _ := cacheByName("artifacts")
	root, err := resolveCacheRoot(data, c)
	if err != nil {
		t.Fatal(err)
	}
	freed, err := clearCacheDir(data, root, nil)
	if err != nil {
		t.Fatalf("clearCacheDir on missing root should be no-op: %v", err)
	}
	if freed != 0 {
		t.Errorf("freed = %d, want 0", freed)
	}
}

// TestClearCacheDir_RefusesOutOfRoot proves clearCacheDir will not remove a
// path outside the data dir even if handed one directly.
func TestClearCacheDir_RefusesOutOfRoot(t *testing.T) {
	data := t.TempDir()
	outside := t.TempDir() // a sibling temp dir, not under data
	mkFile(t, filepath.Join(outside, "precious"), 10)

	if _, err := clearCacheDir(data, outside, nil); err == nil {
		t.Fatal("expected refusal to clear a path outside the data dir")
	}
	// The out-of-root file must still exist.
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Errorf("out-of-root file was touched: %v", err)
	}
}

// TestClearCacheDir_RefusesDataRoot proves the data dir root itself can never
// be cleared (guards against an empty Rel nuking everything).
func TestClearCacheDir_RefusesDataRoot(t *testing.T) {
	data := t.TempDir()
	root := mustAbs(t, data)
	if _, err := clearCacheDir(data, root, nil); err == nil {
		t.Fatal("expected refusal to clear the data dir root itself")
	}
}

func TestCacheByName_SelectsCorrectEntry(t *testing.T) {
	c, ok := cacheByName("gomod")
	if !ok {
		t.Fatal("gomod cache should exist")
	}
	if c.Rel != filepath.Join("cache", "gomod") {
		t.Errorf("gomod Rel = %q, want cache/gomod", c.Rel)
	}
	if _, ok := cacheByName("does-not-exist"); ok {
		t.Error("unknown cache name should return ok=false")
	}
}

func TestManagedCaches_UniqueNamesAndRels(t *testing.T) {
	names := map[string]struct{}{}
	rels := map[string]struct{}{}
	for _, c := range managedCaches() {
		if c.Name == "" || c.Rel == "" {
			t.Errorf("cache has empty name or rel: %+v", c)
		}
		if _, dup := names[c.Name]; dup {
			t.Errorf("duplicate cache name %q", c.Name)
		}
		names[c.Name] = struct{}{}
		if _, dup := rels[c.Rel]; dup {
			t.Errorf("duplicate cache rel %q", c.Rel)
		}
		rels[c.Rel] = struct{}{}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs %q: %v", p, err)
	}
	return abs
}

// TestIsUnder_Windows exercises backslash separators explicitly so the guard
// is proven correct on the platform where ephemerd's data dir is
// C:\ProgramData\ephemerd.
func TestIsUnder_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-specific separators")
	}
	root := `C:\ProgramData\ephemerd`
	if !isUnder(root+`\images`, root) {
		t.Error("images should be under root on windows")
	}
	if isUnder(`C:\ProgramData\ephemerd-evil`, root) {
		t.Error("sibling dir must not be under root on windows")
	}
}
