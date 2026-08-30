//go:build race

package booking_test

// raceDetectorEnabled reports whether this binary was built with -race.
//
// The race detector instruments every memory access and costs a large,
// unpredictable multiple of wall-clock time. That is exactly what a latency
// target must not be measured through: under -race the enriched 409 p99 lands
// around 160 ms against a 150 ms target, which says nothing about the shipped
// binary and everything about the instrumentation.
//
// So the timing assertion is reported rather than enforced under -race. The
// correctness assertions in the same test — exactly one winner, no unclassified
// errors, alternatives actually attached — still run, and they are the ones the
// race detector is there to check.
const raceDetectorEnabled = true
