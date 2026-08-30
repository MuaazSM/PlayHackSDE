package concurrency_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestConcurrentBooking_SingleWinner is the test this entire codebase exists to
// pass.
//
// 500 goroutines, 500 distinct users, one court, one 18:00 slot, released
// together. All three assertions must hold:
//
//  1. exactly one success
//  2. exactly 499 clean ErrSlotTaken — NOT unclassified errors, NOT panics.
//     A run where 499 requests fail with a generic 500 also yields db_count == 1
//     and is a FAILURE. The losers are the majority experience; they have to
//     lose correctly, not merely fail.
//  3. exactly one CONFIRMED row, read back from the database
//
// Distinct users matter: with one user, the idempotency index rather than the
// exclusion constraint would be doing the rejecting, and the test would pass
// while proving nothing about Mechanism A.
func TestConcurrentBooking_SingleWinner(t *testing.T) {
	const n = 500

	pg := testutil.Postgres(t)

	court := testutil.CourtID()
	start, end := testutil.Slot18()

	users := pg.Users(t, n)
	require.Len(t, users, n)

	// Warm the catalogue and the pool so the race measures contention on the
	// GiST index, not cache misses and connection setup.
	cat := testutil.Catalogue(t, pg)
	testutil.WarmCatalogue(t, cat, court)
	svc := pg.BookingServiceWith(t, cat)
	pg.Warm(t, 25)

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		return svc.Create(ctx, booking.CreateRequest{
			FacilityID: court,
			UserID:     users[i],
			Start:      start,
			Duration:   time.Hour,
			IdemKey:    uuid.NewString(),
		})
	})

	confirmed := len(out.Successes())
	conflicts := out.CountIs(booking.ErrSlotTaken)

	// Anything that is neither a win nor a clean conflict is a defect. Surface
	// it loudly rather than letting it hide inside the loser count.
	var unclassified []error
	for _, a := range out.Failures() {
		if !errors.Is(a.Err, booking.ErrSlotTaken) {
			unclassified = append(unclassified, a.Err)
		}
	}

	// Assertion 3: the invariant, read back from the database.
	var dbCount int
	require.NoError(t, pg.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM bookings
		 WHERE facility_id = $1
		   AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')
		   AND status = 'CONFIRMED'`,
		court, start, end).Scan(&dbCount))

	t.Logf("confirmed=%d conflicts=%d db_count=%d", confirmed, conflicts, dbCount)
	t.Logf("%s  reject_p99=%s  confirm=%s",
		out.Summarise(),
		testutil.Percentile(out.Failures(), 99),
		testutil.Percentile(out.Successes(), 50))

	require.Empty(t, unclassified,
		"%d losers failed with something other than ErrSlotTaken; first: %v",
		len(unclassified), firstOrNil(unclassified))

	require.Equal(t, 1, confirmed, "exactly one goroutine may win")
	require.Equal(t, n-1, conflicts, "every loser must be a clean conflict")
	require.Equal(t, 1, dbCount, "the database must hold exactly one confirmed booking")

	// The race must actually have been a race.
	require.Less(t, out.StartSpread, 5*time.Second,
		"goroutines were not released together (spread %s)", out.StartSpread)
}

func firstOrNil(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// TestCreate_DifferentFacilitiesDoNotContend proves the constraint is scoped per
// facility: contention on Tennis Court 1 must not slow or block Cricket Ground.
//
// 500 requests spread over the six exclusive venues must produce exactly six
// winners — one per facility — not one winner overall.
func TestCreate_DifferentFacilitiesDoNotContend(t *testing.T) {
	const n = 500

	pg := testutil.Postgres(t)

	facilities := testutil.ExclusiveFacilityIDs()
	require.Len(t, facilities, 6, "the seed must provide six exclusive venues")

	start, _ := testutil.Slot18()
	users := pg.Users(t, n)

	cat := testutil.Catalogue(t, pg)
	testutil.WarmCatalogue(t, cat, facilities...)
	svc := pg.BookingServiceWith(t, cat)
	pg.Warm(t, 25)

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		return svc.Create(ctx, booking.CreateRequest{
			FacilityID: facilities[i%len(facilities)],
			UserID:     users[i],
			Start:      start,
			Duration:   time.Hour,
			IdemKey:    uuid.NewString(),
		})
	})

	confirmed := len(out.Successes())
	conflicts := out.CountIs(booking.ErrSlotTaken)

	var unclassified []error
	for _, a := range out.Failures() {
		if !errors.Is(a.Err, booking.ErrSlotTaken) {
			unclassified = append(unclassified, a.Err)
		}
	}
	require.Empty(t, unclassified, "first unclassified: %v", firstOrNil(unclassified))

	require.Equal(t, len(facilities), confirmed,
		"one winner per facility — %d facilities should yield %d winners",
		len(facilities), len(facilities))
	require.Equal(t, n-len(facilities), conflicts)

	// And exactly one winner on each individual facility, not six on one.
	for _, f := range facilities {
		var perFacility int
		require.NoError(t, pg.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM bookings WHERE facility_id = $1 AND status = 'CONFIRMED'`,
			f).Scan(&perFacility))
		require.Equalf(t, 1, perFacility, "facility %s must have exactly one booking", f)
	}

	t.Logf("confirmed=%d conflicts=%d facilities=%d %s",
		confirmed, conflicts, len(facilities), out.Summarise())
}
