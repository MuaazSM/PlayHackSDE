package testutil

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// Attempt is one goroutine's outcome in a race.
type Attempt struct {
	Index    int
	Value    any
	Err      error
	Started  time.Time
	Duration time.Duration
}

// Outcome is the collected result of a race.
type Outcome struct {
	Attempts []Attempt

	// StartSpread is the gap between the first and last goroutine actually
	// entering fn. It is the honest measure of whether this was a race or a
	// trickle: if it is large relative to fn's duration, the goroutines queued
	// rather than collided and any "exactly one winner" result is meaningless.
	StartSpread time.Duration

	// Elapsed is wall-clock from release to the last goroutine finishing.
	Elapsed time.Duration
}

// Race runs fn in n goroutines released simultaneously, and collects the result,
// error and duration of each.
//
// Release-together is the whole point. Every goroutine parks on a shared channel
// and does no work until it is closed; closing is a broadcast, so all n wake at
// once. Spawning goroutines that begin work as they are created would produce a
// trickle, and a trickle proves nothing about a concurrency guarantee: the
// winners would simply be whoever started first.
//
// Two mechanisms, doing different jobs:
//
//   - The channel close is what enforces the barrier. It happens after the spawn
//     loop, so every goroutine exists and is blocked before any is released.
//     This is the load-bearing part, and the part the tests can prove.
//   - parked.Wait additionally ensures each goroutine has been scheduled and has
//     reached the receive, rather than merely existing. That only tightens start
//     skew under scheduler pressure — it is cheap insurance, not the guarantee.
//
// Callers should warm the connection pool first (see PG.Warm), or the race
// measures connection setup rather than contention.
func Race(t *testing.T, n int, fn func(ctx context.Context, i int) (any, error)) Outcome {
	t.Helper()

	if n <= 0 {
		t.Fatalf("testutil: Race needs n > 0, got %d", n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var (
		parked  sync.WaitGroup // every goroutine has reached the start line
		done    sync.WaitGroup // every goroutine has finished
		release = make(chan struct{})
		// Each goroutine owns exactly one slot, so no lock is needed and the
		// race detector stays quiet.
		attempts = make([]Attempt, n)
	)
	parked.Add(n)
	done.Add(n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()

			parked.Done() // "I am at the start line"
			<-release     // ...and I do not move until everyone else is too

			started := time.Now()
			value, err := fn(ctx, i)
			attempts[i] = Attempt{
				Index:    i,
				Value:    value,
				Err:      err,
				Started:  started,
				Duration: time.Since(started),
			}
		}(i)
	}

	parked.Wait() // all n are blocked on <-release

	startedAt := time.Now()
	close(release) // one broadcast, everyone moves
	done.Wait()
	elapsed := time.Since(startedAt)

	return newOutcome(attempts, elapsed)
}

func newOutcome(attempts []Attempt, elapsed time.Duration) Outcome {
	o := Outcome{Attempts: attempts, Elapsed: elapsed}

	first, last := attempts[0].Started, attempts[0].Started
	for _, a := range attempts[1:] {
		if a.Started.Before(first) {
			first = a.Started
		}
		if a.Started.After(last) {
			last = a.Started
		}
	}
	o.StartSpread = last.Sub(first)
	return o
}

// Successes returns the attempts that returned no error.
func (o Outcome) Successes() []Attempt { return o.filter(func(a Attempt) bool { return a.Err == nil }) }

// Failures returns the attempts that returned an error.
func (o Outcome) Failures() []Attempt { return o.filter(func(a Attempt) bool { return a.Err != nil }) }

// ErrorsIs returns the attempts whose error matches target.
func (o Outcome) ErrorsIs(target error) []Attempt {
	return o.filter(func(a Attempt) bool { return errors.Is(a.Err, target) })
}

// CountIs is the number of attempts whose error matches target.
func (o Outcome) CountIs(target error) int { return len(o.ErrorsIs(target)) }

func (o Outcome) filter(keep func(Attempt) bool) []Attempt {
	var out []Attempt
	for _, a := range o.Attempts {
		if keep(a) {
			out = append(out, a)
		}
	}
	return out
}

// Percentile returns the p-th percentile duration across the given attempts,
// with p in [0,100]. Used to assert the latency targets: rejections p99 under
// 150 ms, confirmations under 250 ms.
func Percentile(attempts []Attempt, p float64) time.Duration {
	if len(attempts) == 0 {
		return 0
	}
	ds := make([]time.Duration, len(attempts))
	for i, a := range attempts {
		ds[i] = a.Duration
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })

	idx := int(float64(len(ds)-1) * p / 100)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(ds) {
		idx = len(ds) - 1
	}
	return ds[idx]
}

// Summarise renders the outcome for test logs — the shape the race demo prints.
func (o Outcome) Summarise() string {
	return fmt.Sprintf("n=%d ok=%d err=%d spread=%s elapsed=%s",
		len(o.Attempts), len(o.Successes()), len(o.Failures()), o.StartSpread, o.Elapsed)
}
