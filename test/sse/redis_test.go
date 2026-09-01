package sse_test

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestRedisDown_BookingStillSucceeds is non-negotiable #3, under test.
//
// The ENTIRE stack points at an address nothing is listening on: the rate
// limiter, the availability cache, the live publisher and the hub. Every one of
// them fails, on every request, for the whole test. The booking still commits,
// because the only thing that decides a booking is a constraint in Postgres.
//
// This is the test that makes "Redis is never authoritative" a fact rather than
// an intention. The moment somebody puts a lock, a reservation or a
// deduplication key in Redis, this fails — which is exactly when it should.
func TestRedisDown_BookingStillSucceeds(t *testing.T) {
	s := newStack(t, withDeadRedis())

	token := s.login(t, "student01")
	start := tomorrowAt(18)

	res := s.book(t, token, testutil.CourtID(), start, 60)
	require.Equal(t, http.StatusCreated, res.status,
		"a booking must not depend on Redis; body was: %s", res.raw)

	// Confirmed in the table, not merely reported as confirmed by the handler.
	require.Equal(t, 1, countBookings(t, s.pg, "CONFIRMED"))

	// And the constraint is still the thing deciding: a second, overlapping
	// attempt loses, with Redis just as dead as it was for the winner.
	other := s.login(t, "student02")
	res = s.book(t, other, testutil.CourtID(), start, 60)
	require.Equal(t, http.StatusConflict, res.status,
		"the exclusion constraint must still reject an overlap with Redis down; body was: %s", res.raw)

	require.Equal(t, 1, countBookings(t, s.pg, "CONFIRMED"))
}

// TestRedisFlushMidRun_SystemStillCorrect wipes Redis repeatedly WHILE a hundred
// bookings are in flight, and requires the outcome to be untouched.
//
// The design being tested is negative: there is nothing in Redis for a FLUSHALL
// to destroy. The cache is derived and refills, the rate-limit counters reset to
// permissive, the pub/sub channels have no backlog to lose. If any part of the
// booking decision had migrated into Redis — a held-slot key, a distributed
// lock, a "seen this idempotency key" marker — wiping it mid-flight would either
// double-book a slot or fail a booking that should have succeeded, and both show
// up here.
//
// A hundred DISTINCT windows rather than a hundred racers for one: the "exactly
// one winner" property is proved by the concurrency suite, and what needs
// proving here is the other half — that the system does not spuriously REJECT
// under a Redis outage. Every one of these must succeed.
func TestRedisFlushMidRun_SystemStillCorrect(t *testing.T) {
	const facilities = 10
	const slotsEach = 10
	const total = facilities * slotsEach

	// Queue depth above the run's own concurrency, so the shedder cannot fire:
	// a 429 from the write-queue bound would look exactly like the spurious
	// rejection this test exists to rule out, and would have nothing to do with
	// Redis. Shedding is the concurrency suite's subject, not this one's.
	s := newStack(t, withWriteQueueDepth(total))

	// Ten one-off exclusive facilities, ten hourly windows each. Seeded courts
	// would not stretch to a hundred non-overlapping windows in one day, and
	// overlapping ones would make a legitimate 409 look like a Redis failure.
	facilityIDs := make([]uuid.UUID, facilities)
	for i := range facilityIDs {
		facilityIDs[i] = s.pg.Facility(t, fmt.Sprintf("flush%02d", i), true, 1)
	}

	// One token per booking. Distinct users keep the fair-use and idempotency
	// paths out of the way, so a failure here can only be about Redis.
	tokens := make([]string, 0, total)
	for i := 0; i < total; i++ {
		tokens = append(tokens, s.login(t, fmt.Sprintf("student%02d", (i%10)+1)))
	}

	s.pg.Warm(t, 20)

	// FLUSHALL on a tight loop for the whole run, so the wipe lands in the
	// middle of the burst rather than politely between requests.
	flushCtx, stopFlushing := context.WithCancel(context.Background())
	var flushes atomic.Int64
	flushing := make(chan struct{})
	go func() {
		defer close(flushing)
		for flushCtx.Err() == nil {
			if err := s.rdb.FlushAll(flushCtx).Err(); err == nil {
				flushes.Add(1)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	out := testutil.Race(t, total, func(ctx context.Context, i int) (any, error) {
		facilityID := facilityIDs[i/slotsEach]
		start := tomorrowAt(6 + i%slotsEach)

		res := s.book(t, tokens[i], facilityID, start, 60)
		if res.status != http.StatusCreated {
			return res.status, fmt.Errorf("attempt %d: status %d: %s", i, res.status, res.raw)
		}
		return res.status, nil
	})

	stopFlushing()
	<-flushing

	// The wipe actually happened, and happened repeatedly. Without this the test
	// could pass by never having flushed at all.
	require.Greater(t, flushes.Load(), int64(3),
		"Redis was not actually flushed during the run; the test proves nothing")

	// Zero errors. Every window was free and every booking should have taken it.
	var failed []error
	for _, a := range out.Attempts {
		if a.Err != nil {
			failed = append(failed, a.Err)
		}
	}
	require.Emptyf(t, failed, "%d of %d bookings failed while Redis was being wiped: %v",
		len(failed), total, failed)

	// Zero double-bookings, asserted against the table rather than against the
	// responses — the responses are what the service claims, the table is what
	// is true (non-negotiable #4: availability is derived from these rows).
	require.Equal(t, total, countBookings(t, s.pg, "CONFIRMED"))
	require.Equal(t, 0, overlappingPairs(t, s.pg),
		"two CONFIRMED bookings overlap on one facility")
}

// overlappingPairs counts CONFIRMED/BLOCKED bookings that share a facility and
// overlap in time — the thing the exclusion constraint exists to make
// impossible. Anything but zero means the constraint was not what decided.
func overlappingPairs(t *testing.T, pg *testutil.PG) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var n int
	require.NoError(t, pg.Pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM bookings a
		  JOIN bookings b
		    ON a.facility_id = b.facility_id
		   AND a.id < b.id
		   AND a.during && b.during
		 WHERE a.status IN ('CONFIRMED', 'BLOCKED')
		   AND b.status IN ('CONFIRMED', 'BLOCKED')`).Scan(&n))
	return n
}
