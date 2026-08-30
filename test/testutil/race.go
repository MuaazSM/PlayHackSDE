package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/demo"
)

// The race harness lives in internal/demo, and this file is a thin test-facing
// wrapper over it.
//
// It was moved there so the race console on stage and the concurrency suite in
// CI run ONE implementation of "release N goroutines together". Two copies would
// drift, and the day they drifted the demo would be proving something different
// from what the CI gate proves — which is the one thing this project cannot
// afford to be vague about.
//
// The dependency points this way round because testutil pulls in testcontainers
// and the `testing` package, and neither belongs in the API binary or in a CLI a
// judge might run.

// Attempt is one goroutine's outcome in a race.
type Attempt = demo.Attempt

// Outcome is the collected result of a race.
type Outcome = demo.Outcome

// Race runs fn in n goroutines released simultaneously, and collects the result,
// error and duration of each. See demo.Race for the semantics — in particular
// why the barrier is a closed channel and what Outcome.StartSpread is for.
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

	out, err := demo.Race(ctx, n, fn)
	if err != nil {
		t.Fatalf("testutil: Race: %v", err)
	}
	return out
}

// Percentile returns the p-th percentile duration across the given attempts,
// with p in [0,100]. Used to assert the latency targets: rejections p99 under
// 150 ms, confirmations under 250 ms.
func Percentile(attempts []Attempt, p float64) time.Duration {
	return demo.Percentile(attempts, p)
}
