// Package httpx holds the HTTP edge: middleware, routing and the error envelope.
// Business logic does not live here.
package httpx

import (
	"context"
	"encoding/binary"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/booking"
)

// Shedder bounds the number of booking writes in flight and rejects the rest
// immediately. IMPLEMENTATION.md §4.6.
//
// Losing fast is a feature, not a degradation. At 6 PM most users lose, so the
// loser path is the majority experience — which is why the rejection target
// (p99 < 150ms) is TIGHTER than the confirmation target (p99 < 250ms).
//
// Depth is roughly 2.5x the PgBouncer backend pool. Above that, queueing only
// converts a fast 429 into a slow 409: the user waits longer to be told the same
// thing, and the slot was gone before they were admitted anyway. Both outcomes
// are a loss; one of them is quick.
//
// This wraps the booking write path only. Reads are not shed — availability is
// cheap, cacheable, and serving it during a burst is what keeps the screen
// honest about who won.
type Shedder struct {
	slots   chan struct{}
	timeout time.Duration

	admitted atomic.Int64
	shed     atomic.Int64
}

// NewShedder builds a shedder admitting at most depth concurrent calls.
//
// timeout, if positive, bounds each admitted call: a pathological wait must not
// outlive its usefulness while holding a slot others could use.
func NewShedder(depth int, timeout time.Duration) *Shedder {
	if depth < 1 {
		depth = 1
	}
	return &Shedder{
		slots:   make(chan struct{}, depth),
		timeout: timeout,
	}
}

// Do runs fn if a slot is free, and returns booking.ErrShed immediately if not.
//
// The rejection path does not block, sleep, or queue — that is the entire point.
// A non-blocking send with a default branch is what makes a shed O(1) instead of
// "wait and then find out".
//
// fn receives a context that may carry the write timeout; it must be used rather
// than the caller's, or the bound is decorative.
func (s *Shedder) Do(ctx context.Context, fn func(context.Context) error) error {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
		s.admitted.Add(1)
	default:
		s.shed.Add(1)
		return booking.ErrShed
	}

	// A caller who has already gone away should not occupy a slot.
	if err := ctx.Err(); err != nil {
		return err
	}

	if s.timeout <= 0 {
		return fn(ctx)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return fn(ctx)
}

// Depth is the configured concurrency bound.
func (s *Shedder) Depth() int { return cap(s.slots) }

// InFlight is the number of calls currently admitted.
func (s *Shedder) InFlight() int { return len(s.slots) }

// Stats returns cumulative admitted and shed counts, for the metrics endpoint
// and the race console.
func (s *Shedder) Stats() (admitted, shed int64) {
	return s.admitted.Load(), s.shed.Load()
}

// retryAfterMin and retryAfterMax bound the jittered Retry-After hint.
const (
	retryAfterMin = 1 * time.Second
	retryAfterMax = 3 * time.Second
)

// RetryAfter returns a jittered Retry-After value for a shed response.
//
// Jitter matters: a fixed hint synchronises every shed client into one retry
// spike, which is the same herd arriving again on a timer.
func RetryAfter() time.Duration {
	return retryAfterMin + time.Duration(rand.Int63n(int64(retryAfterMax-retryAfterMin+1)))
}

// RetryAfterFor is the deterministic variant, seeded by a request-scoped value
// such as an idempotency key, for tests and reproducible demos.
func RetryAfterFor(seed []byte) time.Duration {
	if len(seed) < 8 {
		var padded [8]byte
		copy(padded[:], seed)
		seed = padded[:]
	}
	span := int64(retryAfterMax - retryAfterMin + 1)
	n := int64(binary.BigEndian.Uint64(seed[:8]) >> 1)
	return retryAfterMin + time.Duration(n%span)
}
