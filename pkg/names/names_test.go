package names

import (
	"strings"
	"testing"
)

func TestGenerate_Format(t *testing.T) {
	for range 100 {
		name := Generate()
		parts := strings.SplitN(name, "_", 2)
		if len(parts) != 2 {
			t.Fatalf("expected format adjective_scientist, got %q", name)
		}
		if parts[0] == "" || parts[1] == "" {
			t.Fatalf("empty component in %q", name)
		}
	}
}

func TestGenerate_UsesKnownWords(t *testing.T) {
	adjSet := make(map[string]bool, len(adjectives))
	for _, a := range adjectives {
		adjSet[a] = true
	}
	sciSet := make(map[string]bool, len(scientists))
	for _, s := range scientists {
		sciSet[s] = true
	}

	for range 200 {
		name := Generate()
		parts := strings.SplitN(name, "_", 2)
		if !adjSet[parts[0]] {
			t.Errorf("adjective %q not in known list", parts[0])
		}
		if !sciSet[parts[1]] {
			t.Errorf("scientist %q not in known list", parts[1])
		}
	}
}

func TestGenerate_Variety(t *testing.T) {
	seen := make(map[string]bool)
	for range 50 {
		seen[Generate()] = true
	}
	// With 36 adjectives × 45 scientists = 1620 combos,
	// 50 calls should produce at least 10 unique names.
	if len(seen) < 10 {
		t.Errorf("expected variety in 50 calls, got only %d unique names", len(seen))
	}
}

func TestGenerate_NonEmpty(t *testing.T) {
	name := Generate()
	if name == "" {
		t.Fatal("Generate() returned empty string")
	}
}

func TestWordLists_NonEmpty(t *testing.T) {
	if len(adjectives) == 0 {
		t.Fatal("adjectives list is empty")
	}
	if len(scientists) == 0 {
		t.Fatal("scientists list is empty")
	}
}

func TestWordLists_NoDuplicates(t *testing.T) {
	seen := make(map[string]bool)
	for _, a := range adjectives {
		if seen[a] {
			t.Errorf("duplicate adjective: %q", a)
		}
		seen[a] = true
	}

	seen = make(map[string]bool)
	for _, s := range scientists {
		if seen[s] {
			t.Errorf("duplicate scientist: %q", s)
		}
		seen[s] = true
	}
}
