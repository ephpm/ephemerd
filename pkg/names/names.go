// Package names generates Docker-style random container names.
// Combines an adjective with a notable scientist/hacker name.
package names

import (
	"fmt"
	"math/rand/v2"
)

var adjectives = []string{
	"bold", "brave", "calm", "cool", "eager", "epic", "fast", "keen",
	"kind", "nice", "pure", "safe", "slim", "sure", "warm", "wise",
	"agile", "happy", "lucid", "noble", "quick", "sharp", "vivid",
	"clever", "gentle", "lively", "mellow", "serene", "steady",
	"bright", "silent", "cosmic", "golden", "mighty", "stoic",
}

var scientists = []string{
	"ada", "bell", "bohr", "cope", "darwin", "edison", "euler",
	"fermi", "gauss", "hopper", "hypatia", "jenner", "kepler",
	"lamarr", "lovelace", "mayer", "newton", "nobel", "noether",
	"pascal", "planck", "raman", "ride", "sagan", "tesla", "turing",
	"volta", "watt", "wright", "wu", "curie", "faraday", "feynman",
	"galileo", "hawking", "heisenberg", "keller", "linus", "mendel",
	"pasteur", "rosalind", "shannon", "swartz", "thompson", "torvalds",
}

// Generate returns a random name in the format "adjective_scientist".
func Generate() string {
	adj := adjectives[rand.IntN(len(adjectives))]
	sci := scientists[rand.IntN(len(scientists))]
	return fmt.Sprintf("%s_%s", adj, sci)
}
