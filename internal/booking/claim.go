package booking

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// statusConfirmed is the state a claim converges on.
const statusConfirmed = "CONFIRMED"

// errNoRowsClaimed is internal: the guarded UPDATE matched nothing. It never
// escapes Claim, which resolves it into convergence or ErrOfferExpired.
var errNoRowsClaimed = errors.New("claim matched no rows")

// Claim accepts a promotion offer: HELD → CONFIRMED. IMPLEMENTATION.md §6.3.
//
// One transaction: authorise the owner, flip the status under a guard that
// includes the deadline, close out the queue entry, record the event.
//
// The court is never released in the middle. HELD and CONFIRMED are both inside
// no_double_book's predicate, so there is no instant during this transition
// where somebody else could take the slot — which is the whole reason a
// promotion offer is a booking row rather than a note on a queue entry.
//
// The deadline is checked by Postgres, in the UPDATE's WHERE clause, not here.
// A claim arriving a millisecond either side of expiry must not be decided by
// whichever API replica's clock happened to serve it.
func (s *Service) Claim(ctx context.Context, bookingID, actorID uuid.UUID) (*Booking, error) {
	if bookingID == uuid.Nil {
		return nil, invalid("booking_id", "required")
	}
	if actorID == uuid.Nil {
		return nil, invalid("actor_id", "required")
	}

	var claimed Booking
	err := store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		// As in Cancel, this load exists ONLY to separate 404 from 403 — two
		// outcomes a single guarded UPDATE collapses into "zero rows". Nothing
		// read here feeds the UPDATE's decision, so a load that goes stale
		// mid-race degrades to zero rows and a conflict, never to a wrong write.
		existing, err := loadBooking(ctx, tx, bookingID)
		if err != nil {
			return err
		}

		// Owner only. A promotion offer is personal — it was made to the student
		// at the head of the queue, and a manager accepting it on their behalf
		// would put somebody on a court they never agreed to turn up to. This is
		// deliberately stricter than Cancel, which a manager may perform.
		if existing.UserID != actorID {
			return fmt.Errorf("%w: offer belongs to another user", ErrForbidden)
		}

		row, fromStatus, err := claimGuarded(ctx, tx, bookingID)
		if errors.Is(err, errNoRowsClaimed) {
			// This call did not perform the claim. Re-read to find out why — by
			// the time the guard matched zero rows, whoever did win (a duplicate
			// claim, or the sweeper) has committed.
			current, loadErr := loadBooking(ctx, tx, bookingID)
			if loadErr != nil {
				return loadErr
			}

			// Already CONFIRMED: a retried claim whose first response was lost.
			// The student's intent is satisfied and the honest answer is their
			// booking (non-negotiable #5). No side effects on this path — the
			// event and the queue update belong to the call that matched.
			if current.Status == statusConfirmed {
				claimed = *current
				claimed.Converged = true
				return nil
			}
			return fmt.Errorf("%w: booking is %s", ErrOfferExpired, current.Status)
		}
		if err != nil {
			return err
		}

		// Close out the queue entry the offer came from.
		//
		// This is the one place internal/booking writes to the waitlist table,
		// and it is deliberate: leaving the entry PROMOTED after its offer was
		// accepted would leave the sweeper looking at a live offer for a court
		// that is already confirmed. Making that depend on optional wiring — the
		// way promotion does — would mean a forgotten dependency shows up as a
		// stuck queue rather than as no queue at all.
		if _, err := tx.Exec(ctx, queries.Get(queries.WaitlistMarkClaimed), bookingID); err != nil {
			return fmt.Errorf("booking: claim: close queue entry: %w", store.Classify(err))
		}

		if err := insertBookingEvent(ctx, tx, bookingID, actorID, &fromStatus, statusConfirmed, "waitlist offer claimed"); err != nil {
			return store.Classify(err)
		}

		// The claimed hold is a confirmation like any other, so it publishes the
		// same topic the create path does — a client should not need to know
		// whether a booking arrived by racing for it or by waiting for it.
		// Inside this transaction, per non-negotiable #7: a claim that loses to
		// the sweeper rolls back and takes this row with it.
		//
		// Deliberately NOT on the converged path above. A retried claim has
		// already been notified about; enqueueing there would push a second
		// "confirmed" for every response that got lost on the way back, which is
		// the noise idempotency exists to prevent.
		if err := outbox.Enqueue(ctx, tx, outbox.TopicBookingConfirmed, map[string]any{
			"booking_id":  row.id,
			"facility_id": row.facilityID,
			"user_id":     row.userID,
			"start":       row.start,
			"end":         row.end,
			"source":      "waitlist_claim",
		}); err != nil {
			return store.Classify(err)
		}

		claimed = Booking{
			ID:         row.id,
			Reference:  Reference(row.id),
			FacilityID: row.facilityID,
			UserID:     row.userID,
			Start:      row.start,
			End:        row.end,
			Status:     statusConfirmed,
			CreatedAt:  row.createdAt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &claimed, nil
}

// claimGuarded runs the status-and-deadline-guarded UPDATE. Zero rows means the
// offer was not claimable: already claimed, already expired, or already swept.
func claimGuarded(ctx context.Context, q store.Querier, bookingID uuid.UUID) (cancelRow, string, error) {
	var (
		row        cancelRow
		fromStatus string
	)

	err := q.QueryRow(ctx, queries.Get(queries.BookingClaimHeld), bookingID).Scan(
		&row.id, &row.facilityID, &row.userID, &row.isExclusive,
		&row.start, &row.end, &row.createdAt, &fromStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, "", errNoRowsClaimed
	}
	if err != nil {
		return row, "", store.Classify(err)
	}
	return row, fromStatus, nil
}
