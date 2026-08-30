//go:build !race

package booking_test

// raceDetectorEnabled is false in an uninstrumented build, where a latency
// measurement means something. See raceflag_on_test.go.
const raceDetectorEnabled = false
