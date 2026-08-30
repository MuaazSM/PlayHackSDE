package booking_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestValidation_PastSlot asserts the rejection AND that it cost nothing.
//
// "Cheap rejections never reach the database" is a performance claim, and §4.2
// leans on it: at 6 PM the herd is large and a malformed request must not touch
// the GiST index. A claim with no test is a wish, so this counts queries.
func TestValidation_PastSlot(t *testing.T) {
	pg := testutil.Postgres(t)

	// Build the service over an instrumented pool, then warm the facility cache
	// so the lookup itself is not counted.
	pool, counter := pg.CountingPool(t)
	cat := facility.NewRepo(pool)
	svc := pg.BookingServiceWith(t, cat)

	court := testutil.CourtID()
	testutil.WarmCatalogue(t, cat, court)

	yesterday, _ := testutil.Slot(18, time.Hour)
	yesterday = yesterday.Add(-24 * time.Hour)

	counter.Reset()

	_, err := svc.Create(context.Background(), booking.CreateRequest{
		FacilityID: court,
		UserID:     testutil.StudentID(0),
		Start:      yesterday,
		Duration:   time.Hour,
		IdemKey:    uuid.NewString(),
	})

	require.ErrorIs(t, err, booking.ErrValidation)
	require.Equal(t, "start", booking.Field(err))

	require.Zero(t, counter.Count(),
		"a slot in the past must be rejected before any database work, issued %d queries",
		counter.Count())

	require.Equal(t, 0, confirmedCount(t, pg, court))
}

// TestValidation_ClockSlack: 60s of slack, so a phone with a slightly wrong
// clock still books rather than being told its slot is in the past.
func TestValidation_ClockSlack(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	court := testutil.CourtID()
	start, _ := testutil.Slot18()

	// Freeze the clock 30s after the slot begins — inside the slack window.
	svc = svc.WithClock(func() time.Time { return start.Add(30 * time.Second) })

	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: court, UserID: testutil.StudentID(0),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err, "30s of clock skew is inside the 60s slack and must be accepted")

	// 90s past is outside it.
	svc2 := pg.BookingService(t).WithClock(func() time.Time { return start.Add(90 * time.Second) })
	_, err = svc2.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.Court2ID(), UserID: testutil.StudentID(1),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.ErrorIs(t, err, booking.ErrValidation)
	require.Equal(t, "start", booking.Field(err))
}

// TestValidation_OutsideHours checks the window against the facility's LOCAL
// day. Tennis Court 1 runs 06:00-22:00 IST.
func TestValidation_OutsideHours(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	court := testutil.CourtID()
	user := testutil.StudentID(0)

	t.Run("before opening", func(t *testing.T) {
		start, _ := testutil.Slot(5, time.Hour) // 05:00 IST, opens 06:00
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: court, UserID: user, Start: start,
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
		require.ErrorIs(t, err, booking.ErrValidation)
		require.Equal(t, "start", booking.Field(err))
	})

	t.Run("ending after closing", func(t *testing.T) {
		start, _ := testutil.Slot(21, time.Hour) // 21:00 + 2h = 23:00, closes 22:00
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: court, UserID: user, Start: start,
			Duration: 2 * time.Hour, IdemKey: uuid.NewString(),
		})
		require.ErrorIs(t, err, booking.ErrValidation)
		require.Equal(t, "duration", booking.Field(err))
	})

	t.Run("ending exactly at closing is allowed", func(t *testing.T) {
		// The window is half-open: a booking ending at 22:00 does not extend
		// past 22:00.
		start, _ := testutil.Slot(21, time.Hour)
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: court, UserID: user, Start: start,
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
		require.NoError(t, err, "21:00-22:00 must be bookable at a court closing at 22:00")
	})

	t.Run("starting exactly at opening is allowed", func(t *testing.T) {
		start, _ := testutil.Slot(6, time.Hour)
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: court, UserID: testutil.StudentID(1), Start: start,
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
		require.NoError(t, err, "06:00-07:00 must be bookable at a court opening at 06:00")
	})
}

// TestValidation_DurationBounds: Tennis Court 1 allows 60-120 minutes.
func TestValidation_DurationBounds(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	court := testutil.CourtID()
	start, _ := testutil.Slot18()

	t.Run("below minimum", func(t *testing.T) {
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: court, UserID: testutil.StudentID(0), Start: start,
			Duration: 30 * time.Minute, IdemKey: uuid.NewString(),
		})
		require.ErrorIs(t, err, booking.ErrValidation)
		require.Equal(t, "duration", booking.Field(err))
	})

	t.Run("above maximum", func(t *testing.T) {
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: court, UserID: testutil.StudentID(0), Start: start,
			Duration: 3 * time.Hour, IdemKey: uuid.NewString(),
		})
		require.ErrorIs(t, err, booking.ErrValidation)
		require.Equal(t, "duration", booking.Field(err))
	})

	t.Run("zero", func(t *testing.T) {
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: court, UserID: testutil.StudentID(0), Start: start,
			Duration: 0, IdemKey: uuid.NewString(),
		})
		require.ErrorIs(t, err, booking.ErrValidation)
	})

	t.Run("at the bounds", func(t *testing.T) {
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: court, UserID: testutil.StudentID(0), Start: start,
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
		require.NoError(t, err, "exactly min_duration must be allowed")

		// Cricket Ground allows up to 180 minutes.
		cricket, _ := testutil.Slot(15, time.Hour)
		_, err = svc.Create(ctx, booking.CreateRequest{
			FacilityID: testutil.FacilityIDBySlug("cricket-ground"),
			UserID:     testutil.StudentID(1), Start: cricket,
			Duration: 3 * time.Hour, IdemKey: uuid.NewString(),
		})
		require.NoError(t, err, "exactly max_duration must be allowed")
	})

	require.Equal(t, 1, confirmedCount(t, pg, court))
}

func TestValidation_InactiveFacility(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	court := testutil.CourtID()

	_, err := pg.Pool.Exec(ctx, `UPDATE facilities SET is_active = false WHERE id = $1`, court)
	require.NoError(t, err)

	// Build the service after the update so the cache reads the current row.
	svc := pg.BookingService(t)

	start, _ := testutil.Slot18()
	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: court, UserID: testutil.StudentID(0), Start: start,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.ErrorIs(t, err, booking.ErrValidation)
	require.Equal(t, "facility_id", booking.Field(err))
	require.Equal(t, 0, confirmedCount(t, pg, court))
}

func TestValidation_UnknownFacility(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)

	start, _ := testutil.Slot18()
	_, err := svc.Create(context.Background(), booking.CreateRequest{
		FacilityID: uuid.New(), UserID: testutil.StudentID(0), Start: start,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.ErrorIs(t, err, booking.ErrNotFound)
}
