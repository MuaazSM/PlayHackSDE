//go:build race

package main

// raceEnabled reports whether this binary was built with -race.
//
// The detector adds several times the per-request cost, so the latency budgets
// are not meaningful under it. TestLoadProfile_ThresholdsPass keeps every
// correctness assertion and drops only the two percentile checks — measuring an
// instrumented binary and calling the result a p99 would be worse than not
// measuring at all. `make load` builds without -race and is where the numbers
// come from.
const raceEnabled = true
