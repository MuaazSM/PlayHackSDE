// Package booking_test exercises the write path against a real Postgres.
//
// The exclusion constraint is the thing under test. A mock cannot raise a 23P01,
// so nothing here is mocked.
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

func confirmedCount(t *testing.T, pg *testutil.PG, facilityID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bookings WHERE facility_id = $1 AND status = 'CONFIRMED'`,
		facilityID).Scan(&n))
	return n
}

func TestCreate_HappyPath(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	start, end := testutil.Slot18()

	b, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(),
		UserID:     testutil.StudentID(0),
		Start:      start,
		Duration:   time.Hour,
		IdemKey:    uuid.NewString(),
	})
	require.NoError(t, err)
	require.NotNil(t, b)

	require.Equal(t, testutil.CourtID(), b.FacilityID)
	require.Equal(t, testutil.StudentID(0), b.UserID)
	require.Equal(t, "CONFIRMED", b.Status)
	require.False(t, b.Replayed)
	require.True(t, b.Start.Equal(start), "start %s != %s", b.Start, start)
	require.True(t, b.End.Equal(end), "end %s != %s", b.End, end)
	require.False(t, b.CreatedAt.IsZero())

	require.Equal(t, 1, confirmedCount(t, pg, testutil.CourtID()))

	// The audit trail is written in the same transaction as the booking.
	var events int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM booking_events WHERE booking_id = $1 AND to_status = 'CONFIRMED'`,
		b.ID).Scan(&events))
	require.Equal(t, 1, events)

	// So is the outbox row. Nothing is sent from the handler.
	var outboxRows int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE topic = 'booking.confirmed'`).Scan(&outboxRows))
	require.Equal(t, 1, outboxRows)
}

func TestCreate_ReturnsBookingReference(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	start, _ := testutil.Slot18()

	b, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(),
		UserID:     testutil.StudentID(0),
		Start:      start,
		Duration:   time.Hour,
		IdemKey:    uuid.NewString(),
	})
	require.NoError(t, err)

	require.NotEmpty(t, b.Reference)
	require.Equal(t, "PH-", b.Reference[:3], "reference should be quotable at the venue desk")
	require.Len(t, b.Reference, 11)

	// Derived from the id, so it cannot drift from the booking it names.
	require.Equal(t, booking.Reference(b.ID), b.Reference)

	// Distinct bookings get distinct references.
	b2, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.Court2ID(),
		UserID:     testutil.StudentID(1),
		Start:      start,
		Duration:   time.Hour,
		IdemKey:    uuid.NewString(),
	})
	require.NoError(t, err)
	require.NotEqual(t, b.Reference, b2.Reference)
}

// TestAdjacentSlotsDoNotCollide is the '[)' bounds guard, at the domain level
// this time. 18:00-19:00 and 19:00-20:00 share an endpoint and must not overlap.
//
// A wrong bound type here produces a failure indistinguishable from the
// constraint working correctly, which is what makes it worth a dedicated test.
func TestAdjacentSlotsDoNotCollide(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	court := testutil.CourtID()
	eighteen, _ := testutil.Slot(18, time.Hour)
	nineteen, _ := testutil.Slot(19, time.Hour)
	seventeen, _ := testutil.Slot(17, time.Hour)

	for i, start := range []time.Time{eighteen, nineteen, seventeen} {
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: court,
			UserID:     testutil.StudentID(i),
			Start:      start,
			Duration:   time.Hour,
			IdemKey:    uuid.NewString(),
		})
		require.NoErrorf(t, err, "adjacent slot starting %s must book cleanly — check '[)' bounds", start)
	}

	require.Equal(t, 3, confirmedCount(t, pg, court))
}

// TestOverlappingPartial covers the case a fixed slot grid would miss entirely:
// a booking that starts inside another one.
func TestOverlappingPartial(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	court := testutil.CourtID()
	eighteen, _ := testutil.Slot(18, time.Hour)

	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: court,
		UserID:     testutil.StudentID(0),
		Start:      eighteen,
		Duration:   time.Hour,
		IdemKey:    uuid.NewString(),
	})
	require.NoError(t, err)

	// 17:30-18:30 overlaps the second half hour of the existing booking.
	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: court,
		UserID:     testutil.StudentID(1),
		Start:      eighteen.Add(-30 * time.Minute),
		Duration:   time.Hour,
		IdemKey:    uuid.NewString(),
	})
	require.ErrorIs(t, err, booking.ErrSlotTaken)

	// 18:30-19:30 overlaps the first half hour.
	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: court,
		UserID:     testutil.StudentID(2),
		Start:      eighteen.Add(30 * time.Minute),
		Duration:   time.Hour,
		IdemKey:    uuid.NewString(),
	})
	require.ErrorIs(t, err, booking.ErrSlotTaken)

	// A two-hour booking that swallows it whole.
	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: court,
		UserID:     testutil.StudentID(3),
		Start:      eighteen.Add(-30 * time.Minute),
		Duration:   2 * time.Hour,
		IdemKey:    uuid.NewString(),
	})
	require.ErrorIs(t, err, booking.ErrSlotTaken)

	require.Equal(t, 1, confirmedCount(t, pg, court))
}

// TestCreate_SharedFacilityUsesMechanismB checks the branch routes to capacity
// accounting rather than to the exclusive insert. A booking on the gym must
// create a counter row; one on a court must not.
func TestCreate_SharedFacilityUsesMechanismB(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	start, _ := testutil.Slot18()

	b, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.GymID(),
		UserID:     testutil.StudentID(0),
		Start:      start,
		Duration:   time.Hour,
		IdemKey:    uuid.NewString(),
	})
	require.NoError(t, err)
	require.Equal(t, "CONFIRMED", b.Status)

	var booked, capacity int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT booked, capacity FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
		testutil.GymID(), start).Scan(&booked, &capacity))
	require.Equal(t, 1, booked)
	require.Equal(t, 30, capacity)

	// The row is not in the exclusion constraint's index, which is what lets a
	// second person book the same hour.
	var isExclusive bool
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT is_exclusive FROM bookings WHERE id = $1`, b.ID).Scan(&isExclusive))
	require.False(t, isExclusive)
}
