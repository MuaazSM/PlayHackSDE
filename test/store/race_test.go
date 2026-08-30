package store_test

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestRaceHelper_ReleasesTogether is the test that protects every later
// concurrency test in this project.
//
// If Race let goroutines start as they were spawned, a "500 requests, exactly
// one winner" result would prove nothing: the winner would simply be whoever
// started first, and the exclusion constraint would never actually be contended.
// The guarantee is that no goroutine enters fn until all n are at the start line.
func TestRaceHelper_ReleasesTogether(t *testing.T) {
	const n = 500

	// inFlight/peak track how many goroutines are inside fn at once.
	var inFlight, peak atomic.Int64

	// minLive is the fewest goroutines alive at any goroutine's ENTRY into fn.
	// This is the direct test of the barrier: no goroutine may enter fn until
	// all n have reached the start line, so every entry must observe at least n
	// live goroutines. Without the barrier the first goroutine can enter while
	// the spawn loop is still creating the rest, and this drops below n.
	var minLive atomic.Int64
	minLive.Store(math.MaxInt64)

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		live := int64(runtime.NumGoroutine())
		for {
			old := minLive.Load()
			if live >= old || minLive.CompareAndSwap(old, live) {
				break
			}
		}

		cur := inFlight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		// Hold briefly so overlap is observable.
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return i, nil
	})

	require.Len(t, out.Attempts, n)
	require.Len(t, out.Successes(), n)

	t.Logf("%s peak_concurrency=%d min_live_goroutines=%d",
		out.Summarise(), peak.Load(), minLive.Load())

	// The barrier itself. If Race closed the release channel before every
	// goroutine was parked, an early goroutine would enter fn while later ones
	// did not yet exist, and minLive would fall below n.
	require.GreaterOrEqual(t, minLive.Load(), int64(n),
		"a goroutine entered fn while only %d were alive; all %d must be at the "+
			"start line before any is released", minLive.Load(), n)

	// Every goroutine must have entered fn within a few ms of the first. This is
	// the release-together guarantee.
	require.Less(t, out.StartSpread, 50*time.Millisecond,
		"goroutines must be released together, not in a trickle (spread was %s)", out.StartSpread)

	// With a 20ms body and a true simultaneous release, essentially all of them
	// should overlap. A trickle would show a peak in the low tens.
	require.Greater(t, peak.Load(), int64(n/2),
		"expected most of the %d goroutines in flight at once, peaked at %d", n, peak.Load())

	// And the whole race should take about as long as one body, not n bodies.
	require.Less(t, out.Elapsed, 2*time.Second,
		"a released-together race should finish in roughly one body duration")
}

// TestRaceHelper_CollectsPerGoroutineResults checks the bookkeeping the
// concurrency suites depend on: which attempt won, which errored, and how long
// each took.
func TestRaceHelper_CollectsPerGoroutineResults(t *testing.T) {
	const n = 50
	sentinel := errors.New("lost")

	// Exactly one winner, chosen deterministically, so the accounting is
	// checkable.
	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		if i == 7 {
			return "won", nil
		}
		return nil, sentinel
	})

	require.Len(t, out.Successes(), 1)
	require.Equal(t, 7, out.Successes()[0].Index)
	require.Equal(t, "won", out.Successes()[0].Value)

	require.Len(t, out.Failures(), n-1)
	require.Equal(t, n-1, out.CountIs(sentinel))
	require.Equal(t, 0, out.CountIs(context.Canceled))

	// Every attempt is accounted for exactly once, with a duration recorded.
	seen := make(map[int]bool, n)
	for _, a := range out.Attempts {
		require.False(t, seen[a.Index], "attempt %d recorded twice", a.Index)
		seen[a.Index] = true
		require.GreaterOrEqual(t, a.Duration, time.Duration(0))
		require.False(t, a.Started.IsZero())
	}
	require.Len(t, seen, n)

	require.GreaterOrEqual(t, testutil.Percentile(out.Attempts, 99), time.Duration(0))
}
