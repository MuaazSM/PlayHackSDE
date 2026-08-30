package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// Counter is the state of one slot's capacity after a take or release.
type Counter struct {
	SlotStart time.Time
	SlotEnd   time.Time
	Booked    int
	Capacity  int
}

// Slot is one grid block of a shared booking.
type Slot struct {
	Start time.Time
	End   time.Time
}

// slotsFor splits [start, end) into granularity-sized blocks, ASCENDING.
//
// Ascending order is not cosmetic. A multi-slot booking takes one counter row
// per slot, and two transactions that lock the same rows in opposite orders
// deadlock. Every caller walking the slots in ascending slot_start means all
// transactions acquire the rows in the same order, which is what makes the
// multi-slot path deadlock-free — the same discipline §4.4 prescribes.
//
// The caller must have validated grid alignment first; this assumes the window
// divides evenly.
func slotsFor(f *facility.Facility, start, end time.Time) []Slot {
	if f.Granularity <= 0 {
		return []Slot{{Start: start, End: end}}
	}

	var slots []Slot
	for cur := start; cur.Before(end); cur = cur.Add(f.Granularity) {
		next := cur.Add(f.Granularity)
		if next.After(end) {
			next = end
		}
		slots = append(slots, Slot{Start: cur, End: next})
	}
	return slots
}

// capacityTake is Mechanism B: claim one place in a slot.
//
// A single statement. It lazily creates the counter row and increments it under
// the cap in one shot, so there is no gap between reading the count and taking a
// place — the same property Mechanism A gets from the exclusion constraint, and
// the reason no SELECT appears anywhere on this path.
//
// ON CONFLICT DO UPDATE takes a row lock for the statement's duration, so
// concurrent increments serialise correctly and each caller sees a distinct
// booked value.
//
// Zero rows returned means the WHERE guard failed, i.e. booked was already at
// capacity: the slot is full.
func capacityTake(
	ctx context.Context,
	q store.Querier,
	facilityID uuid.UUID,
	slot Slot,
	capacity int,
) (Counter, error) {
	c := Counter{SlotStart: slot.Start, SlotEnd: slot.End}

	err := q.QueryRow(ctx, queries.Get(queries.CapacityTake),
		facilityID, slot.Start, slot.End, capacity,
	).Scan(&c.Booked, &c.Capacity)

	if errors.Is(err, pgx.ErrNoRows) {
		// Full. Not a database failure — a business outcome, and the one the
		// majority of a 6 PM gym rush will get.
		return c, fmt.Errorf("%w: slot %s", ErrCapacityFull, slot.Start.Format(time.RFC3339))
	}
	if err != nil {
		return c, err
	}
	return c, nil
}

// capacityRelease gives a place back. Used by cancel and by the no-show sweep.
//
// The booked > 0 guard is what stops a double cancel driving the counter
// negative. Zero rows returned is a no-op, not an error: releasing a slot that
// was never taken, or was already released, is exactly what a retried
// cancellation looks like and must be harmless.
func capacityRelease(
	ctx context.Context,
	q store.Querier,
	facilityID uuid.UUID,
	slot Slot,
) (Counter, bool, error) {
	c := Counter{SlotStart: slot.Start, SlotEnd: slot.End}

	err := q.QueryRow(ctx, queries.Get(queries.CapacityRelease),
		facilityID, slot.Start,
	).Scan(&c.Booked, &c.Capacity)

	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	return c, true, nil
}

// ReleaseCapacity returns every place a shared booking held, inside the caller's
// transaction.
//
// Exported for the cancel path and the no-show sweep, which must release in the
// same transaction that changes the booking's status — a release committed
// separately from the cancellation is a place that leaks if either half fails.
//
// Walks slots in ascending order, for the same reason capacityTake does.
func ReleaseCapacity(
	ctx context.Context,
	q store.Querier,
	f *facility.Facility,
	facilityID uuid.UUID,
	start, end time.Time,
) ([]Counter, error) {
	if f.IsExclusive {
		// Exclusive facilities have no counter rows; the exclusion constraint
		// releases the slot when the row's status changes.
		return nil, nil
	}

	var out []Counter
	for _, slot := range slotsFor(f, start, end) {
		c, released, err := capacityRelease(ctx, q, facilityID, slot)
		if err != nil {
			return nil, fmt.Errorf("booking: release %s: %w", slot.Start.Format(time.RFC3339), err)
		}
		if released {
			out = append(out, c)
		}
	}
	return out, nil
}
