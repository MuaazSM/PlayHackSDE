package demo_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/demo"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestRaceDemo_OneWinnerAtN500 is the demo's own version of the test this
// codebase exists to pass.
//
// It asserts the same three things TestConcurrentBooking_SingleWinner does, but
// through the surface a judge will actually watch: one winner, 499 clean
// conflicts, one row in the database. If this is green and the console is on
// screen, the 45 seconds work.
func TestRaceDemo_OneWinnerAtN500(t *testing.T) {
	h := newHarness(t)

	res := h.race(t, 500)

	require.Equal(t, 500, res.N)
	require.Equal(t, 1, res.Confirmed, "exactly one attempt may win")
	require.Equal(t, 499, res.Conflict409, "every loser must be a clean 409")
	require.Zerof(t, res.Other,
		"%d attempts failed with something other than a conflict: %v", res.Other, res.Errors)
	require.Equal(t, 1, res.DBCount, "the database must hold exactly one confirmed booking")

	// The winner is read back from the database, so it must name a real seeded
	// student rather than whatever the winning goroutine believed.
	require.NotNil(t, res.Winner)
	require.NotEmpty(t, res.Winner.User)
	require.Contains(t, res.Winner.Reference, "PH-")

	// And it must actually have been a race, not a trickle.
	require.Less(t, res.StartSpreadMS, int64(5000),
		"goroutines were not released together (spread %d ms)", res.StartSpreadMS)

	t.Logf("confirmed=%d conflicts=%d db_count=%d elapsed=%dms p50=%dms p99=%dms winner=%s",
		res.Confirmed, res.Conflict409, res.DBCount,
		res.ElapsedMS, res.P50MS, res.P99MS, res.Winner.User)
}

// TestRaceDemo_DBCountMatchesConfirmedCount checks the console cannot report a
// pretty number that the database disagrees with.
//
// db_count and confirmed are computed by completely different means — one is a
// tally of what 500 goroutines returned, the other is a fresh SELECT issued
// after they all finished. The demo's entire claim is that those two agree. A
// third, independently written count runs here as the tiebreaker, so a helper
// that counted the wrong thing cannot agree with itself.
func TestRaceDemo_DBCountMatchesConfirmedCount(t *testing.T) {
	h := newHarness(t)

	res := h.race(t, 200)

	require.Equal(t, res.Confirmed, res.DBCount,
		"the goroutines' tally and the database's count must agree")
	require.Equal(t, res.DBCount, h.confirmedInSlot(t, h.court),
		"an independently written count must agree with the one the console reports")
	require.Equal(t, res.N, res.Confirmed+res.Conflict409+res.Other,
		"every attempt must be accounted for in exactly one bucket")
}

// TestRaceDemo_Repeatable is the "fire again" beat, and the reason reset exists.
//
// Three fires and one reset, in the order the runbook uses them:
//
//	fire            -> 1 winner, db_count 1
//	fire again      -> 0 winners, everyone conflicts, db_count STILL 1
//	reset           -> db_count 0
//	fire again      -> 1 winner, db_count 1
//
// The second fire is the most convincing beat on stage and the one most likely
// to be skipped in testing: it proves the constraint keeps rejecting long after
// the race that created the winner is over.
func TestRaceDemo_Repeatable(t *testing.T) {
	h := newHarness(t)

	first := h.race(t, 100)
	require.Equal(t, 1, first.Confirmed)
	require.Equal(t, 1, first.DBCount)
	winner := first.Winner.BookingID

	second := h.race(t, 100)
	require.Zero(t, second.Confirmed, "the slot is taken; nobody may win it twice")
	require.Equal(t, 100, second.Conflict409)
	require.Equal(t, 1, second.DBCount, "still exactly one booking")
	require.Equal(t, winner, second.Winner.BookingID, "and still the same booking")

	cleared := h.reset(t)
	require.Equal(t, 1, cleared.Cancelled)
	require.Zero(t, cleared.DBCount, "reset must leave the slot empty")

	third := h.race(t, 100)
	require.Equal(t, 1, third.Confirmed, "the slot is bookable again after a reset")
	require.Equal(t, 99, third.Conflict409)
	require.Equal(t, 1, third.DBCount)
	require.NotEqual(t, winner, third.Winner.BookingID, "a new race means a new booking")
}

// TestRaceDemo_ReportsLatencyPercentiles checks the telemetry is real rather
// than zero-filled, and internally consistent.
//
// The numbers underneath the proof are what make the demo a measurement instead
// of an anecdote, so a silently broken percentile would be worse than none.
func TestRaceDemo_ReportsLatencyPercentiles(t *testing.T) {
	h := newHarness(t)

	res := h.race(t, 200)

	require.Positive(t, res.ElapsedMS, "a 200-way race takes measurable time")
	require.GreaterOrEqual(t, res.P99MS, res.P50MS, "p99 cannot be below p50")
	require.LessOrEqual(t, res.P99MS, res.ElapsedMS,
		"no single attempt can outlast the whole race")
	require.GreaterOrEqual(t, res.RejectP99MS, int64(0))
	require.GreaterOrEqual(t, res.StartSpreadMS, int64(0))

	t.Logf("elapsed=%dms p50=%dms p99=%dms reject_p99=%dms spread=%dms",
		res.ElapsedMS, res.P50MS, res.P99MS, res.RejectP99MS, res.StartSpreadMS)
}

// TestRaceDemo_HandlesN1AndN1000 walks the boundaries.
//
// n=1 is the degenerate race with nobody to lose to; n=1000 is twice the
// rehearsed size, which is what a presenter types when the first run looks too
// easy. Neither may panic, and both must still produce exactly one row.
// Non-positive and absurd values must be refused as requests, not attempted.
func TestRaceDemo_HandlesN1AndN1000(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx(t)

	t.Run("n=1", func(t *testing.T) {
		res := h.race(t, 1)
		require.Equal(t, 1, res.Confirmed)
		require.Zero(t, res.Conflict409)
		require.Zero(t, res.Other)
		require.Equal(t, 1, res.DBCount)
	})

	h.reset(t)

	t.Run("n=1000", func(t *testing.T) {
		res := h.race(t, 1000)
		require.Equal(t, 1, res.Confirmed)
		require.Equal(t, 999, res.Conflict409)
		require.Zerof(t, res.Other, "unclassified failures: %v", res.Errors)
		require.Equal(t, 1, res.DBCount)
	})

	t.Run("n=0 and n<0 are refused, not attempted", func(t *testing.T) {
		for _, n := range []int{0, -1} {
			_, err := h.svc.Run(ctx, demo.Request{
				FacilityID: h.court, Start: h.start, Duration: time.Hour, N: n,
			})
			require.ErrorIs(t, err, demo.ErrInvalid, "n=%d", n)
		}
	})

	t.Run("above MaxN is refused", func(t *testing.T) {
		_, err := h.svc.Run(ctx, demo.Request{
			FacilityID: h.court, Start: h.start, Duration: time.Hour, N: demo.MaxN + 1,
		})
		require.ErrorIs(t, err, demo.ErrInvalid)
	})
}

// TestRaceDemo_NoLeakedGoroutines proves the barrier joins everyone it starts.
//
// This matters more here than in a test binary that exits straight afterwards:
// the same runner backs POST /api/v1/demo/race, and a console fired a dozen
// times during a rehearsal would otherwise accumulate thousands of parked
// goroutines inside a long-lived API process.
func TestRaceDemo_NoLeakedGoroutines(t *testing.T) {
	h := newHarness(t)

	// One race first, so the pool has opened whatever connections and background
	// goroutines it wants. Measuring around a cold pool would count those as
	// leaks.
	h.race(t, 50)
	h.reset(t)

	before := settledGoroutines()
	h.race(t, 200)
	after := settledGoroutines()

	// A small tolerance, not zero: the runtime and the pgx pool are free to keep
	// their own workers around. What must not survive is anything proportional
	// to the 200 goroutines just spawned.
	require.LessOrEqual(t, after, before+10,
		"race leaked goroutines: %d before, %d after 200 attempts", before, after)
}

// settledGoroutines reads the goroutine count after giving finished goroutines a
// chance to be reaped, so the assertion is not a race of its own.
func settledGoroutines() int {
	prev := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		runtime.GC()
		n := runtime.NumGoroutine()
		if n >= prev {
			return n
		}
		prev = n
	}
	return prev
}

// TestRaceDemo_ResetClearsSlot covers the operation the presenter leans on
// between takes.
//
// Reset must leave the slot genuinely bookable — which for an exclusive court
// means the row drops out of the exclusion constraint's predicate, and for the
// shared gym means the capacity counter goes back down. It must also leave an
// audit trail: a booking that vanished with no record of why is exactly the kind
// of thing this schema refuses to do everywhere else.
func TestRaceDemo_ResetClearsSlot(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx(t)

	t.Run("exclusive court", func(t *testing.T) {
		res := h.race(t, 20)
		require.Equal(t, 1, res.DBCount)
		booked := res.Winner.BookingID

		cleared := h.reset(t)
		require.Equal(t, 1, cleared.Cancelled)
		require.Zero(t, cleared.DBCount)
		require.Zero(t, h.confirmedInSlot(t, h.court))

		var status string
		require.NoError(t, h.pg.Pool.QueryRow(ctx,
			`SELECT status::text FROM bookings WHERE id = $1`, booked).Scan(&status))
		require.Equal(t, "CANCELLED", status, "the row must be cancelled, not deleted")

		// The audit trail says what happened and that it was the demo.
		var events int
		require.NoError(t, h.pg.Pool.QueryRow(ctx, `
			SELECT count(*) FROM booking_events
			 WHERE booking_id = $1 AND to_status = 'CANCELLED' AND reason = 'demo reset'`,
			booked).Scan(&events))
		require.Equal(t, 1, events)
	})

	t.Run("shared gym gives its capacity back", func(t *testing.T) {
		bookings := h.pg.BookingService(t)

		users := testutil.StudentIDs()
		for i := 0; i < 3; i++ {
			_, err := bookings.Create(ctx, booking.CreateRequest{
				FacilityID: h.gym,
				UserID:     users[i],
				Start:      h.start,
				Duration:   time.Hour,
				IdemKey:    "gym-" + time.Now().Format(time.RFC3339Nano) + string(rune('a'+i)),
			})
			require.NoError(t, err)
		}

		require.Equal(t, 3, bookedCount(t, h, h.gym))

		cleared, err := h.svc.Reset(ctx, h.gym, h.start, h.end)
		require.NoError(t, err)
		require.Equal(t, 3, cleared.Cancelled)
		require.Zero(t, cleared.DBCount)
		require.Zero(t, bookedCount(t, h, h.gym),
			"Mechanism B keeps a counter rather than deriving occupancy, so a reset "+
				"that forgot to release it would leave the gym permanently fuller than it is")
	})

	t.Run("resetting an empty slot is a no-op, not an error", func(t *testing.T) {
		cleared := h.reset(t)
		require.Zero(t, cleared.Cancelled)
		require.Zero(t, cleared.DBCount)
	})

	t.Run("an inverted window is refused rather than silently clearing nothing", func(t *testing.T) {
		_, err := h.svc.Reset(ctx, h.court, h.end, h.start)
		require.ErrorIs(t, err, demo.ErrInvalid)
	})
}

func bookedCount(t *testing.T, h *harness, facilityID uuid.UUID) int {
	t.Helper()
	var booked int
	err := h.pg.Pool.QueryRow(context.Background(),
		`SELECT coalesce(sum(booked), 0) FROM slot_capacity
		  WHERE facility_id = $1 AND slot_start = $2`,
		facilityID, h.start).Scan(&booked)
	require.NoError(t, err)
	return booked
}

// TestRaceDemo_WorksWithRedisDown is the one CLAUDE.md's risk register cares
// about most: "Demo fails on venue wifi — race console fully in-process, local
// DB".
//
// Two halves, because one alone would not be enough:
//
//  1. A live run with every Redis client in the process pointed at a dead
//     address. The race must still produce exactly one winner.
//  2. A static check that internal/demo does not so much as import Redis, HTTP,
//     or the test harness. The first half proves it works today; the second
//     stops someone making it depend on Redis tomorrow, which is the failure
//     mode that would only be discovered on stage.
func TestRaceDemo_WorksWithRedisDown(t *testing.T) {
	pg := testutil.Postgres(t)
	cat := testutil.Catalogue(t, pg)
	court := testutil.CourtID()
	testutil.WarmCatalogue(t, cat, court)
	pg.Warm(t, 20)

	// Everything Redis-shaped in the process points at a closed port.
	dead := testutil.DeadRedis()
	t.Cleanup(func() { _ = dead.Close() })

	availability := facility.NewAvailability(pg.DB.Replica, dead, "Asia/Kolkata", nil)
	bookings := booking.NewService(pg.DB, cat, testutil.IST).
		WithAlternatives(booking.NewAlternatives(pg.DB.Replica, availability, "Asia/Kolkata"))

	svc := demo.NewService(pg.DB, bookings, cat)
	start, _ := futureSlot()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := svc.Run(ctx, demo.Request{
		FacilityID: court, Start: start, Duration: time.Hour, N: 100,
	})
	require.NoError(t, err, "a dead Redis must not break the race console")
	require.Equal(t, 1, res.Confirmed)
	require.Equal(t, 99, res.Conflict409)
	require.Zerof(t, res.Other, "unclassified failures: %v", res.Errors)
	require.Equal(t, 1, res.DBCount)

	t.Run("internal/demo depends on nothing that can be unplugged", func(t *testing.T) {
		forbidden := map[string]string{
			"redis":             "Redis is never authoritative and the demo must not need it at all",
			"net/http":          "the race calls the domain service directly; no HTTP belongs in this package",
			"test/testutil":     "that would link testcontainers into the API binary and the CLI",
			"testcontainers":    "the demo runs against the local database, not a container it starts",
			"net/http/httptest": "same reason as net/http",
		}

		for _, imp := range packageImports(t, "internal/demo") {
			for bad, why := range forbidden {
				require.NotContainsf(t, imp, bad,
					"internal/demo imports %q — %s", imp, why)
			}
		}
	})
}

// TestRaceDemo_RunRejectsUnknownFacility checks the demo fails loudly on a
// misconfigured request rather than reporting n identical errors.
func TestRaceDemo_RunRejectsUnknownFacility(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Run(h.ctx(t), demo.Request{
		FacilityID: uuid.New(), Start: h.start, Duration: time.Hour, N: 10,
	})
	require.ErrorIs(t, err, demo.ErrInvalid)
	require.False(t, errors.Is(err, booking.ErrSlotTaken))
}
