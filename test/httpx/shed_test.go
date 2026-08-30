// Package httpx_test covers the HTTP edge middleware.
//
// The shedder needs no database: it is pure concurrency control, and testing it
// against Postgres would only make it slower and less deterministic.
package httpx_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

func TestShedder_AdmitsUpToDepth(t *testing.T) {
	const depth = 4
	s := httpx.NewShedder(depth, 0)

	release := make(chan struct{})
	var wg sync.WaitGroup

	// Occupy every slot.
	for i := 0; i < depth; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Do(context.Background(), func(context.Context) error {
				<-release
				return nil
			})
		}()
	}

	require.Eventually(t, func() bool { return s.InFlight() == depth }, 2*time.Second, time.Millisecond)

	// The next one is refused without waiting.
	start := time.Now()
	err := s.Do(context.Background(), func(context.Context) error {
		t.Fatal("must not run while the shedder is full")
		return nil
	})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, booking.ErrShed)
	require.Less(t, elapsed, 20*time.Millisecond,
		"a shed must be immediate, not a queue with a timeout — it took %s", elapsed)

	close(release)
	wg.Wait()

	// Slots are returned, so the next call is admitted.
	require.NoError(t, s.Do(context.Background(), func(context.Context) error { return nil }))
	require.Equal(t, 0, s.InFlight())
}

// TestShedder_NeverExceedsDepth is the invariant that matters: the bound must
// hold under a simultaneous burst, not just sequentially.
func TestShedder_NeverExceedsDepth(t *testing.T) {
	const (
		depth = 8
		n     = 500
	)
	s := httpx.NewShedder(depth, 0)

	var inFlight, peak atomic.Int64

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		return nil, s.Do(ctx, func(context.Context) error {
			cur := inFlight.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
			return nil
		})
	})

	admitted := len(out.Successes())
	shed := out.CountIs(booking.ErrShed)

	require.LessOrEqual(t, peak.Load(), int64(depth),
		"concurrency exceeded the bound: peaked at %d with depth %d", peak.Load(), depth)
	require.Equal(t, n, admitted+shed, "every request must be admitted or shed, never lost")
	require.Positive(t, shed, "a burst of %d against depth %d must shed", n, depth)

	statsAdmitted, statsShed := s.Stats()
	require.Equal(t, int64(admitted), statsAdmitted)
	require.Equal(t, int64(shed), statsShed)

	t.Logf("depth=%d n=%d admitted=%d shed=%d peak=%d", depth, n, admitted, shed, peak.Load())
}

// TestShedder_ShedIsFasterThanAdmitted is the §4.6 claim stated as a test:
// losing must be cheaper than winning.
func TestShedder_ShedIsFasterThanAdmitted(t *testing.T) {
	const (
		depth = 4
		n     = 200
	)
	s := httpx.NewShedder(depth, 0)

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		return nil, s.Do(ctx, func(context.Context) error {
			time.Sleep(10 * time.Millisecond) // the "work"
			return nil
		})
	})

	var admitted, shedAttempts []testutil.Attempt
	for _, a := range out.Attempts {
		if errors.Is(a.Err, booking.ErrShed) {
			shedAttempts = append(shedAttempts, a)
		} else {
			admitted = append(admitted, a)
		}
	}
	require.NotEmpty(t, shedAttempts)
	require.NotEmpty(t, admitted)

	shedP99 := testutil.Percentile(shedAttempts, 99)
	admitP99 := testutil.Percentile(admitted, 99)

	t.Logf("shed_p99=%s admitted_p99=%s", shedP99, admitP99)
	require.Less(t, shedP99, admitP99, "shedding must be cheaper than doing the work")
	require.Less(t, shedP99, 10*time.Millisecond, "a shed should cost approximately nothing")
}

func TestShedder_AppliesWriteTimeout(t *testing.T) {
	s := httpx.NewShedder(4, 30*time.Millisecond)

	err := s.Do(context.Background(), func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return errors.New("the timeout did not fire")
		}
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// A slot occupied by a timed-out call must be returned.
	require.Equal(t, 0, s.InFlight())
}

func TestShedder_DoesNotRunWorkForACancelledCaller(t *testing.T) {
	s := httpx.NewShedder(4, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ran bool
	err := s.Do(ctx, func(context.Context) error {
		ran = true
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, ran, "a caller who has gone away must not occupy a slot doing work")
	require.Equal(t, 0, s.InFlight())
}

func TestRetryAfter_IsJitteredWithinBounds(t *testing.T) {
	seen := make(map[time.Duration]int)
	for i := 0; i < 200; i++ {
		d := httpx.RetryAfter()
		require.GreaterOrEqual(t, d, time.Second)
		require.LessOrEqual(t, d, 3*time.Second)
		seen[d]++
	}
	// A fixed hint would synchronise every shed client into one retry spike —
	// the same herd arriving again on a timer.
	require.Greater(t, len(seen), 1, "Retry-After must be jittered, not constant")
}
