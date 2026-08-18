//go:build race

package dind

// raceDetectorEnabled reports whether this test binary was built with -race.
// Go exposes no runtime API for this, so it is derived from the build tag the
// toolchain sets for us.
const raceDetectorEnabled = true
