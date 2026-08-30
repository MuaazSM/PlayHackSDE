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

// TestIdempotentReplay: the same key twice returns the same booking and creates
// one row.
//
// The subtlety worth stating: on the replay the INSERT raises 23505, which
// ABORTS the transaction. No further query may run on that connection until
// rollback. The replay lookup therefore has to happen after the rollback, on a
// fresh connection. Getting that wrong looks like a flaky test and is not — it
// is the aborted-transaction rule in §4.5.
func TestIdempotentReplay(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	court := testutil.CourtID()
	user := testutil.StudentID(0)
	key := uuid.NewString()
	start, _ := testutil.Slot18()

	req := booking.CreateRequest{
		FacilityID: court,
		UserID:     user,
		Start:      start,
		Duration:   time.Hour,
		IdemKey:    key,
	}

	first, err := svc.Create(ctx, req)
	require.NoError(t, err)
	require.False(t, first.Replayed, "the first submit creates the booking")

	second, err := svc.Create(ctx, req)
	require.NoError(t, err, "a replay is a success, not a conflict")
	require.True(t, second.Replayed, "the caller must be able to tell 200 from 201")

	require.Equal(t, first.ID, second.ID, "a replay must return the original booking")
	require.Equal(t, first.Reference, second.Reference)
	require.Equal(t, first.UserID, second.UserID)
	require.True(t, first.Start.Equal(second.Start))
	require.Equal(t, key, second.IdemKey)

	require.Equal(t, 1, confirmedCount(t, pg, court), "a replay must not create a second row")

	// A third replay behaves the same — the property is not one-shot.
	third, err := svc.Create(ctx, req)
	require.NoError(t, err)
	require.True(t, third.Replayed)
	require.Equal(t, first.ID, third.ID)
	require.Equal(t, 1, confirmedCount(t, pg, court))
}

// TestIdempotentReplay_Concurrent fires the same key from ten goroutines at once.
//
// Whichever index the losers hit — uq_bookings_user_idem or no_double_book, and
// which one fires first is not guaranteed — every caller must end up with the
// same booking, and the database must hold exactly one row.
func TestIdempotentReplay_Concurrent(t *testing.T) {
	pg := testutil.Postgres(t)

	court := testutil.CourtID()
	user := testutil.StudentID(0)
	key := uuid.NewString()
	start, _ := testutil.Slot18()

	// Warm the exact catalogue the service will consult, and the pool, so the
	// race measures contention rather than cache misses and connection setup.
	cat := testutil.Catalogue(t, pg)
	testutil.WarmCatalogue(t, cat, court)
	svc := pg.BookingServiceWith(t, cat)
	pg.Warm(t, 10)

	const n = 10
	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		return svc.Create(ctx, booking.CreateRequest{
			FacilityID: court,
			UserID:     user,
			Start:      start,
			Duration:   time.Hour,
			IdemKey:    key,
		})
	})

	require.Len(t, out.Failures(), 0,
		"every concurrent submit with the same key must resolve to the same booking, not an error")

	ids := make(map[uuid.UUID]int)
	created, replayed := 0, 0
	for _, a := range out.Attempts {
		b := a.Value.(*booking.Booking)
		ids[b.ID]++
		if b.Replayed {
			replayed++
		} else {
			created++
		}
	}

	require.Len(t, ids, 1, "all %d goroutines must see one booking id, saw %d distinct", n, len(ids))
	require.Equal(t, 1, created, "exactly one goroutine may create the booking")
	require.Equal(t, n-1, replayed, "the other %d must be replays", n-1)

	require.Equal(t, 1, confirmedCount(t, pg, court), "exactly one row in the database")

	t.Logf("created=%d replayed=%d db_count=%d spread=%s",
		created, replayed, confirmedCount(t, pg, court), out.StartSpread)
}

// TestIdempotency_DifferentKeysAreDifferentIntentions guards the other
// direction: idempotency must not collapse two genuinely different submits.
func TestIdempotency_DifferentKeysAreDifferentIntentions(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	user := testutil.StudentID(0)
	eighteen, _ := testutil.Slot(18, time.Hour)
	twenty, _ := testutil.Slot(20, time.Hour)

	first, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: user, Start: eighteen,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	second, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: user, Start: twenty,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	require.NotEqual(t, first.ID, second.ID)
	require.False(t, second.Replayed)
	require.Equal(t, 2, confirmedCount(t, pg, testutil.CourtID()))
}
