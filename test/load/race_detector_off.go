//go:build !race

package main

// raceEnabled reports whether this binary was built with -race. See the
// //go:build race twin of this file.
const raceEnabled = false
