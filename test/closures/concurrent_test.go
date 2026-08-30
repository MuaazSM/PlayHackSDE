package closures_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// contenders is how many students fight the closure for the slot.
//
// Not 500. This is not the race gate — test/concurrency owns that — and what is
// being proved here is a different claim: that a closure and a booking contend
// through the SAME mechanism, so one of them loses. Twenty goroutines released
// together is enough contention to exercise it and cheap enough to run beside
// nine other closure tests.
const contenders = 20

// TestClosure_ConcurrentWithBooking fires a closure and a burst of bookings at
// the same window, at the same instant.
//
// The guarantee for an EXCLUSIVE facility is absolute and the assertion is made
// against the table, not against what the services returned: after the dust
// settles exactly ONE row holds the window. It may be the closure or it may be a
// student — the exclusion constraint does not care which arrived first, only that
// there is one — and there is no interleaving in which a student holds a court
// that is also closed.
//
// The closure gets no special treatment to achieve this. It is an INSERT into
// bookings competing on no_double_book with every other INSERT, which is the
// entire argument for making a closure a booking row instead of a table of its
// own: a second table would need a second mechanism to keep the two in step, and
// that mechanism would be application code deciding a race.
func TestClosure_ConcurrentWithBooking(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)

	// Warm the pool, or the race measures connection setup instead of contention.
	r.pg.Warm(t, contenders+1)
	users := r.pg.Users(t, contenders)

	// Index 0 is the closure; the rest are students. All released together.
	out := testutil.Race(t, contenders+1, func(ctx context.Context, i int) (any, error) {
		if i == 0 {
			return r.svc.CreateClosure(ctx, booking.ClosureRequest{
				FacilityID: r.court,
				ActorID:    r.manager(),
				Start:      start,
				End:        end,
				Reason:     "emergency",
			})
		}
		return r.svc.Create(ctx, booking.CreateRequest{
			FacilityID: r.court,
			UserID:     users[i-1],
			Start:      start,
			Duration:   time.Hour,
			IdemKey:    uuidString(),
		})
	})

	var winners, conflicts, other int
	closureWon := false
	for _, a := range out.Attempts {
		switch {
		case a.Err == nil:
			winners++
			if a.Index == 0 {
				closureWon = true
			}
		case errors.Is(a.Err, booking.ErrSlotTaken):
			conflicts++
		default:
			other++
		}
	}

	require.Zero(t, other, "every loser must lose with a conflict, not a fault")
	require.Equal(t, 1, winners, "exactly one of the closure and %d bookings may win", contenders)
	require.Equal(t, contenders, conflicts)

	// THE ASSERTION THAT MATTERS. Derived from the rows, because the rows are the
	// only authority on who won.
	require.Equal(t, 1, overlapping(t, r.pg, r.court, start, end),
		"exactly one row may hold the window, closure or booking")

	// And the outcome is coherent either way round: if the closure won the court
	// reads closed, and if a student won it reads booked. Never both.
	if closureWon {
		require.Equal(t, "closed", stateAt(t, r, r.court, start))
	} else {
		require.Equal(t, "booked", stateAt(t, r, r.court, start))
	}
}

// TestClosure_ConcurrentWithBookingOnGym is the same race on the SHARED facility,
// where the guarantee is necessarily different and worth stating explicitly.
//
// The exclusion constraint does not cover these rows, so a closure and a booking
// do NOT exclude each other: bookings that commit before the counter is zeroed
// stand, and are handed to staff. What must hold — and what this asserts — is
// that the two contend on the SAME counter row, so once the closure has committed
// no further booking can be admitted. An interleaving where a booking slips in
// after the closure is exactly the silent failure §10.4 warns about.
func TestClosure_ConcurrentWithBookingOnGym(t *testing.T) {
	r := newRig(t)
	start, _ := slot(18, time.Hour)

	r.pg.Warm(t, contenders+1)
	users := r.pg.Users(t, contenders)

	out := testutil.Race(t, contenders+1, func(ctx context.Context, i int) (any, error) {
		if i == 0 {
			return r.svc.CreateClosure(ctx, booking.ClosureRequest{
				FacilityID: r.gym,
				ActorID:    r.manager(),
				Start:      start,
				End:        start.Add(time.Hour),
				Reason:     "emergency",
			})
		}
		return r.svc.Create(ctx, booking.CreateRequest{
			FacilityID: r.gym,
			UserID:     users[i-1],
			Start:      start,
			Duration:   time.Hour,
			IdemKey:    uuidString(),
		})
	})

	confirmed := 0
	for _, a := range out.Attempts {
		if a.Index == 0 {
			require.NoError(t, a.Err, "the closure itself must not fail")
			continue
		}
		if a.Err == nil {
			confirmed++
			continue
		}
		require.ErrorIs(t, a.Err, booking.ErrCapacityFull,
			"a booking that lost to the closure lost on capacity, not on a fault")
	}

	// The counter agrees with the bookings table: no place was taken without a
	// row, and no row exists without a place.
	capacity, booked, found := capacityOf(t, r.pg, r.gym, start)
	require.True(t, found)
	require.Equal(t, 0, capacity, "the slot is closed once the closure has committed")
	require.Equal(t, confirmed, booked)

	var rows int
	require.NoError(t, r.pg.Pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM bookings
		  WHERE facility_id = $1 AND status = 'CONFIRMED'
		    AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')`,
		r.gym, start, start.Add(time.Hour)).Scan(&rows))
	require.Equal(t, confirmed, rows)

	// Whatever the interleaving was, the gym is shut NOW.
	_, err := r.book(r.gym, testutil.StudentID(0), start, time.Hour)
	require.ErrorIs(t, err, booking.ErrCapacityFull)
}
