//go:build !race

package dind

// raceDetectorEnabled reports whether this test binary was built with -race.
// See racedetector_race_test.go.
const raceDetectorEnabled = false
