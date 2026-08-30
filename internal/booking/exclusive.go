package booking

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
)

// lockClassBooking namespaces the advisory lock so it cannot collide with any
// other advisory-lock user in this database.
const lockClassBooking = 1

// lockFacility serialises write attempts on one facility for the life of the
// transaction.
//
// It is a contention shaper, not the concurrency control. The exclusion
// constraint still decides who wins; this only stops conflicting inserters from
// racing inside the GiST index, where each places its index tuple before
// scanning and two of them can end up waiting on each other. See the .sql file.
//
// Keyed per facility, so a burst on Tennis Court 1 never delays Cricket Ground.
func lockFacility(ctx context.Context, q store.Querier, facilityID uuid.UUID) error {
	_, err := q.Exec(ctx, queries.Get(queries.BookingLockFacility),
		lockClassBooking, facilityKey(facilityID))
	return err
}

// facilityKey folds a facility id into the int32 an advisory lock takes.
//
// A collision would only make two facilities share a queue — slower, never
// incorrect — and with a catalogue of seven venues it will not happen.
func facilityKey(id uuid.UUID) int32 {
	return int32(binary.BigEndian.Uint32(id[:4]))
}

// insertExclusive is Mechanism A, and it is the entire concurrency control for
// exclusive facilities.
//
// A plain INSERT. There is no SELECT before it and there must never be one:
// Postgres serialises conflicting inserts on the GiST index behind
// no_double_book, and every loser raises 23P01. A read-then-write would put a
// gap between the check and the insert, and that gap is the bug the whole design
// exists to eliminate.
//
// The error is returned raw for the caller to classify. This function does not
// interpret it, and it does not retry: a 23P01 means the caller lost, and the
// loser must lose fast.
// insertShared writes the booking row for a shared facility.
//
// is_exclusive is false, which keeps the row out of the no_double_book
// constraint entirely — occupancy here was already decided by capacityTake.
func insertShared(
	ctx context.Context,
	q store.Querier,
	facilityID, userID uuid.UUID,
	start, end time.Time,
	idemKey *string,
) (id uuid.UUID, createdAt time.Time, err error) {
	err = q.QueryRow(ctx, queries.Get(queries.BookingInsertShared),
		facilityID, userID, start, end, idemKey,
	).Scan(&id, &createdAt)
	return id, createdAt, err
}

func insertExclusive(
	ctx context.Context,
	q store.Querier,
	facilityID, userID uuid.UUID,
	start, end time.Time,
	idemKey *string,
) (id uuid.UUID, createdAt time.Time, err error) {
	err = q.QueryRow(ctx, queries.Get(queries.BookingInsertExclusive),
		facilityID, userID, start, end, idemKey,
	).Scan(&id, &createdAt)
	return id, createdAt, err
}
