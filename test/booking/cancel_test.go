package booking_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

func mustCreate(t *testing.T, svc *booking.Service, facility, user uuid.UUID, start time.Time, d time.Duration) *booking.Booking {
	t.Helper()
	b, err := svc.Create(context.Background(), booking.CreateRequest{
		FacilityID: facility, UserID: user, Start: start,
		Duration: d, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)
	return b
}

func statusOf(t *testing.T, pg *testutil.PG, id uuid.UUID) string {
	t.Helper()
	var s string
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT status::text FROM bookings WHERE id = $1`, id).Scan(&s))
	return s
}

// TestCancel_ReleasesSlot is non-negotiable #4 in action: there is no "release"
// step, so the only way to know the slot is free is that somebody else can book
// it. Asserting the status changed would prove nothing about availability.
func TestCancel_ReleasesSlot(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	court := testutil.CourtID()
	start, _ := testutil.Slot18()

	first := mustCreate(t, svc, court, testutil.StudentID(0), start, time.Hour)

	// While it stands, the slot is taken.
	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: court, UserID: testutil.StudentID(1), Start: start,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.ErrorIs(t, err, booking.ErrSlotTaken)

	cancelled, err := svc.Cancel(ctx, first.ID, testutil.StudentID(0), "changed my mind")
	require.NoError(t, err)
	require.Equal(t, "CANCELLED", cancelled.Status)
	require.Equal(t, first.ID, cancelled.ID)

	// The row is still there — cancellation is a status transition, not a delete.
	require.Equal(t, "CANCELLED", statusOf(t, pg, first.ID))

	// And the slot is bookable again, with no release step having run.
	second, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: court, UserID: testutil.StudentID(1), Start: start,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err, "cancelling must drop the row out of the constraint predicate")
	require.NotEqual(t, first.ID, second.ID)
}

func TestCancel_DoubleCancelIsRejected(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	start, _ := testutil.Slot18()
	b := mustCreate(t, svc, testutil.CourtID(), testutil.StudentID(0), start, time.Hour)

	_, err := svc.Cancel(ctx, b.ID, testutil.StudentID(0), "")
	require.NoError(t, err)

	// A second cancel must not report success. Telling a user their booking was
	// cancelled when this call cancelled nothing is a lie they cannot detect.
	_, err = svc.Cancel(ctx, b.ID, testutil.StudentID(0), "")
	require.ErrorIs(t, err, booking.ErrNotCancellable)

	// Distinct from a booking that never existed.
	_, err = svc.Cancel(ctx, uuid.New(), testutil.StudentID(0), "")
	require.ErrorIs(t, err, booking.ErrNotFound)
	require.NotErrorIs(t, err, booking.ErrNotCancellable)

	var events int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM booking_events WHERE booking_id = $1 AND to_status = 'CANCELLED'`,
		b.ID).Scan(&events))
	require.Equal(t, 1, events, "a rejected double cancel must not append an event")
}

func TestCancel_NotOwnerForbidden(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	start, _ := testutil.Slot18()
	b := mustCreate(t, svc, testutil.CourtID(), testutil.StudentID(0), start, time.Hour)

	_, err := svc.Cancel(ctx, b.ID, testutil.StudentID(1), "not mine")
	require.ErrorIs(t, err, booking.ErrForbidden)

	require.Equal(t, "CONFIRMED", statusOf(t, pg, b.ID), "a forbidden cancel must change nothing")

	// An actor who does not exist at all is forbidden, not a 500.
	_, err = svc.Cancel(ctx, b.ID, uuid.New(), "")
	require.ErrorIs(t, err, booking.ErrForbidden)
	require.Equal(t, "CONFIRMED", statusOf(t, pg, b.ID))
}

func TestCancel_ManagerCanCancelAnyBooking(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	start, _ := testutil.Slot18()
	owner := testutil.StudentID(0)
	b := mustCreate(t, svc, testutil.CourtID(), owner, start, time.Hour)

	manager := testutil.UserIDByRoll("manager01")
	cancelled, err := svc.Cancel(ctx, b.ID, manager, "facility maintenance")
	require.NoError(t, err)
	require.Equal(t, "CANCELLED", cancelled.Status)

	// The booking still belongs to the student; the manager is only the actor.
	require.Equal(t, owner, cancelled.UserID)

	var actor uuid.UUID
	var from, to string
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT actor_id, from_status::text, to_status::text FROM booking_events
		  WHERE booking_id = $1 AND to_status = 'CANCELLED'`, b.ID).Scan(&actor, &from, &to))
	require.Equal(t, manager, actor, "the audit trail must record who actually cancelled it")
	require.Equal(t, "CONFIRMED", from)
	require.Equal(t, "CANCELLED", to)

	// A secretary is not a manager.
	b2 := mustCreate(t, svc, testutil.Court2ID(), owner, start, time.Hour)
	_, err = svc.Cancel(ctx, b2.ID, testutil.UserIDByRoll("secretary01"), "")
	require.ErrorIs(t, err, booking.ErrForbidden)
}

func TestCancel_SharedReleasesCapacity(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	gym := testutil.GymID()
	start, _ := testutil.Slot18()

	b := mustCreate(t, svc, gym, testutil.StudentID(0), start, time.Hour)

	var booked int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT booked FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
		gym, start.UTC()).Scan(&booked))
	require.Equal(t, 1, booked)

	_, err := svc.Cancel(ctx, b.ID, testutil.StudentID(0), "")
	require.NoError(t, err)

	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT booked FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
		gym, start.UTC()).Scan(&booked))
	require.Equal(t, 0, booked, "a shared cancel must give the place back")

	// The counter row survives at zero; it is a counter, not a lock.
	require.Equal(t, "CANCELLED", statusOf(t, pg, b.ID))

	// And the place is genuinely reusable.
	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: gym, UserID: testutil.StudentID(1), Start: start,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT booked FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
		gym, start.UTC()).Scan(&booked))
	require.Equal(t, 1, booked)
}

// TestCancel_ConcurrentDoubleCancel: the status guard is the concurrency
// control, so two simultaneous cancels must produce exactly one winner — and the
// capacity counter must be decremented exactly once, not twice.
func TestCancel_ConcurrentDoubleCancel(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	cat := testutil.Catalogue(t, pg)
	testutil.WarmCatalogue(t, cat, testutil.CourtID(), testutil.GymID())
	svc := pg.BookingServiceWith(t, cat)
	pg.Warm(t, 10)

	t.Run("exclusive", func(t *testing.T) {
		start, _ := testutil.Slot18()
		b := mustCreate(t, svc, testutil.CourtID(), testutil.StudentID(0), start, time.Hour)

		out := testutil.Race(t, 2, func(ctx context.Context, i int) (any, error) {
			return svc.Cancel(ctx, b.ID, testutil.StudentID(0), "")
		})

		require.Len(t, out.Successes(), 1, "exactly one cancel may win")
		require.Equal(t, 1, out.CountIs(booking.ErrNotCancellable), "the loser must get a clean 409")

		var events int
		require.NoError(t, pg.Pool.QueryRow(ctx,
			`SELECT count(*) FROM booking_events WHERE booking_id = $1 AND to_status = 'CANCELLED'`,
			b.ID).Scan(&events))
		require.Equal(t, 1, events, "only the winning cancel may append an event")
	})

	t.Run("shared decrements exactly once", func(t *testing.T) {
		pg.Reset(t)
		cat.InvalidateAll()
		testutil.WarmCatalogue(t, cat, testutil.GymID())

		gym := testutil.GymID()
		start, _ := testutil.Slot18()

		// Two bookings, so a double decrement would show as 0 instead of 1.
		keep := mustCreate(t, svc, gym, testutil.StudentID(0), start, time.Hour)
		drop := mustCreate(t, svc, gym, testutil.StudentID(1), start, time.Hour)
		require.NotEqual(t, keep.ID, drop.ID)

		out := testutil.Race(t, 8, func(ctx context.Context, i int) (any, error) {
			return svc.Cancel(ctx, drop.ID, testutil.StudentID(1), "")
		})
		require.Len(t, out.Successes(), 1)
		require.Equal(t, 7, out.CountIs(booking.ErrNotCancellable))

		var booked int
		require.NoError(t, pg.Pool.QueryRow(ctx,
			`SELECT booked FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
			gym, start.UTC()).Scan(&booked))
		require.Equal(t, 1, booked,
			"the losing cancels must not each decrement the counter")
	})
}

func TestBookingEvents_RecordedForCreateAndCancel(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	start, _ := testutil.Slot18()
	b := mustCreate(t, svc, testutil.CourtID(), testutil.StudentID(0), start, time.Hour)

	_, err := svc.Cancel(ctx, b.ID, testutil.StudentID(0), "changed my mind")
	require.NoError(t, err)

	rows, err := pg.Pool.Query(ctx, `
		SELECT from_status::text, to_status::text, coalesce(reason,'')
		  FROM booking_events WHERE booking_id = $1 ORDER BY id`, b.ID)
	require.NoError(t, err)
	defer rows.Close()

	type ev struct{ from, to, reason string }
	var events []ev
	for rows.Next() {
		var e ev
		var from *string
		require.NoError(t, rows.Scan(&from, &e.to, &e.reason))
		if from != nil {
			e.from = *from
		}
		events = append(events, e)
	}
	require.NoError(t, rows.Err())

	require.Len(t, events, 2, "the trail must be complete from the first write onward")

	require.Empty(t, events[0].from, "a creation has no previous status")
	require.Equal(t, "CONFIRMED", events[0].to)
	require.Equal(t, "created", events[0].reason)

	require.Equal(t, "CONFIRMED", events[1].from, "the transition must record where it came from")
	require.Equal(t, "CANCELLED", events[1].to)
	require.Equal(t, "changed my mind", events[1].reason)
}

func TestListMine_OrdersAndFiltersCorrectly(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	me := testutil.StudentID(0)
	someoneElse := testutil.StudentID(1)
	court := testutil.CourtID()

	base, _ := testutil.Slot(12, time.Hour)

	yesterday := base.Add(-24 * time.Hour)
	earlier := base.Add(-2 * time.Hour)
	inProgressStart := base.Add(-30 * time.Minute)
	soon := base.Add(2 * time.Hour)
	later := base.Add(4 * time.Hour)

	// Create everything from a clock that sits before all of it — Create
	// legitimately refuses to book the past, so the fixtures cannot be made from
	// the vantage point the listing is asserted from.
	svc = svc.WithClock(func() time.Time { return yesterday.Add(-time.Hour) })

	past1 := mustCreate(t, svc, court, me, yesterday, time.Hour)
	past2 := mustCreate(t, svc, court, me, earlier, time.Hour)
	next1 := mustCreate(t, svc, court, me, soon, time.Hour)
	next2 := mustCreate(t, svc, court, me, later, time.Hour)
	inProgress := mustCreate(t, svc, testutil.FacilityIDBySlug("badminton-court-1"),
		me, inProgressStart, time.Hour)

	// Another student's booking, and one of mine that I cancel.
	other := mustCreate(t, svc, testutil.Court2ID(), someoneElse, soon, time.Hour)
	mine := mustCreate(t, svc, testutil.Court2ID(), me, later, time.Hour)
	_, err := svc.Cancel(ctx, mine.ID, me, "")
	require.NoError(t, err)

	// Now stand at noon and ask what the student sees.
	svc = svc.WithClock(func() time.Time { return base })

	list, err := svc.ListMine(ctx, me)
	require.NoError(t, err)

	ids := func(bs []booking.Booking) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.ID)
		}
		return out
	}

	// A booking in progress counts as upcoming: the student is at the court now
	// and it is the row they need on screen.
	require.Equal(t, []uuid.UUID{inProgress.ID, next1.ID, next2.ID}, ids(list.Upcoming),
		"upcoming must be soonest first, and must include a booking in progress")
	require.Equal(t, []uuid.UUID{past2.ID, past1.ID}, ids(list.Past),
		"past must be most recent first")

	require.NotContains(t, ids(list.Upcoming), other.ID, "another student's booking must not appear")
	require.NotContains(t, ids(list.Upcoming), mine.ID, "a cancelled booking must not appear")
	require.NotContains(t, ids(list.Past), mine.ID)

	// The screen needs the facility name, so the list query supplies it.
	require.Equal(t, "Badminton Court 1", list.Upcoming[0].FacilityName)
	require.Equal(t, "Tennis Court 1", list.Upcoming[1].FacilityName)
	require.NotEmpty(t, list.Upcoming[0].Reference)
}
