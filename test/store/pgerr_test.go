package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// TestClassify_ExclusionViolation generates a REAL 23P01 from the real schema
// and checks it maps to ErrSlotTaken.
func TestClassify_ExclusionViolation(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	court := testutil.CourtID()
	start, end := testutil.Slot18()
	users := testutil.StudentIDs()

	var id uuid.UUID
	require.NoError(t, pg.Pool.QueryRow(ctx, insertBooking,
		court, users[0], true, start, end, nil).Scan(&id))

	// Same court, same window, different user.
	_, rawErr := pg.Pool.Exec(ctx, insertBooking, court, users[1], true, start, end, nil)
	require.Error(t, rawErr)

	// Sanity: this really is the constraint firing, not something else.
	var pgErr *pgconn.PgError
	require.True(t, errors.As(rawErr, &pgErr))
	require.Equal(t, "23P01", pgErr.Code)
	require.Equal(t, "no_double_book", pgErr.ConstraintName)

	err := store.Classify(rawErr)
	require.ErrorIs(t, err, store.ErrSlotTaken)
	require.True(t, store.IsClassified(err))

	// The underlying driver error stays reachable for logging.
	var classified *store.Error
	require.True(t, errors.As(err, &classified))
	require.Equal(t, "23P01", classified.Code)
	require.Equal(t, "no_double_book", classified.Constraint)

	var recovered *pgconn.PgError
	require.True(t, errors.As(err, &recovered), "the original PgError must survive classification")
	require.Equal(t, pgErr.Code, recovered.Code)
}

// TestClassify_IdemDuplicate generates a REAL 23505 on uq_bookings_user_idem.
//
// The two bookings use non-overlapping slots on purpose: if they overlapped, the
// exclusion constraint would fire first and this would silently be testing
// 23P01 instead.
func TestClassify_IdemDuplicate(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	court := testutil.CourtID()
	user := testutil.StudentID(0)
	key := uuid.NewString()

	start18, end18 := testutil.Slot(18, time.Hour) // 18:00-19:00
	start20, end20 := testutil.Slot(20, time.Hour) // 20:00-21:00, no overlap

	var id uuid.UUID
	require.NoError(t, pg.Pool.QueryRow(ctx, insertBooking,
		court, user, true, start18, end18, key).Scan(&id))

	_, rawErr := pg.Pool.Exec(ctx, insertBooking, court, user, true, start20, end20, key)
	require.Error(t, rawErr)

	var pgErr *pgconn.PgError
	require.True(t, errors.As(rawErr, &pgErr))
	require.Equal(t, "23505", pgErr.Code)
	require.Equal(t, "uq_bookings_user_idem", pgErr.ConstraintName,
		"must be the idempotency index, not the exclusion constraint")

	err := store.Classify(rawErr)
	require.ErrorIs(t, err, store.ErrIdempotentReplay)
	require.NotErrorIs(t, err, store.ErrSlotTaken)
	require.NotErrorIs(t, err, store.ErrAlreadyWaiting)
}

// TestClassify_DistinguishesConstraints is the one that matters most in this
// file. Two 23505s mean two different things: an idempotent replay is a 200 with
// the original booking, a duplicate waitlist entry is a 409. Matching on the
// code alone would collapse them and return the wrong thing to a real user.
func TestClassify_DistinguishesConstraints(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	court := testutil.CourtID()
	user := testutil.StudentID(0)
	start, end := testutil.Slot18()
	during := testutil.TSTZRange(start, end)

	const insertWaitlist = `
		INSERT INTO waitlist (facility_id, user_id, during, status)
		VALUES ($1, $2, $3::tstzrange, 'WAITING')
		RETURNING id`

	var id uuid.UUID
	require.NoError(t, pg.Pool.QueryRow(ctx, insertWaitlist, court, user, during).Scan(&id))

	// Same user, same facility, same slot, still WAITING.
	_, rawErr := pg.Pool.Exec(ctx, insertWaitlist, court, user, during)
	require.Error(t, rawErr)

	var pgErr *pgconn.PgError
	require.True(t, errors.As(rawErr, &pgErr))
	require.Equal(t, "23505", pgErr.Code, "same SQLSTATE as the idempotency clash")
	require.Equal(t, "uq_waitlist_live", pgErr.ConstraintName)

	err := store.Classify(rawErr)
	require.ErrorIs(t, err, store.ErrAlreadyWaiting)

	// The whole point: identical code, different constraint, different meaning.
	require.NotErrorIs(t, err, store.ErrIdempotentReplay,
		"a 23505 on uq_waitlist_live must never be read as an idempotent replay")
	require.NotErrorIs(t, err, store.ErrSlotTaken)
}

// TestClassify_UnknownIsNotGuessed covers the other half of the contract: an
// error with no mapping must stay unclassified rather than be forced into the
// nearest sentinel. An unclassified error is a 500, which is the honest answer.
func TestClassify_UnknownIsNotGuessed(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	start, end := testutil.Slot18()

	// 23503 — foreign key violation from a booking on a nonexistent facility.
	_, rawErr := pg.Pool.Exec(ctx, insertBooking,
		uuid.New(), testutil.StudentID(0), true, start, end, nil)
	require.Error(t, rawErr)

	err := store.Classify(rawErr)
	require.NotErrorIs(t, err, store.ErrSlotTaken)
	require.NotErrorIs(t, err, store.ErrIdempotentReplay)
	require.NotErrorIs(t, err, store.ErrAlreadyWaiting)
	require.False(t, store.IsClassified(err), "an unmapped error must not be classified")

	// Still inspectable for logs.
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))

	// And nil in, nil out.
	require.NoError(t, store.Classify(nil))
}

// TestClassify_TimeoutMapsToErrTimeout covers the non-SQLSTATE row of the §4.5
// table.
func TestClassify_TimeoutMapsToErrTimeout(t *testing.T) {
	err := store.Classify(context.DeadlineExceeded)
	require.ErrorIs(t, err, store.ErrTimeout)
	require.NotErrorIs(t, err, store.ErrSlotTaken)
}
