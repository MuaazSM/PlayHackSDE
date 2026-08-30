package booking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// Promoter is the waitlist's half of a cancellation, declared here so the
// dependency points one way: booking knows there is a queue, and internal/
// waitlist knows about bookings. waitlist.Service satisfies it.
//
// The method takes a pgx.Tx rather than a pool BECAUSE it must run inside the
// cancelling transaction — the same reason outbox.Enqueue does. A promotion
// that could be committed on its own would eventually be committed on its own,
// leaving a student holding a court whose original booking never went away.
//
// Its contract, which Cancel relies on: whatever it returns, this transaction
// is still usable. An implementation that cannot promise that must return an
// error rather than leaving an aborted transaction behind.
type Promoter interface {
	Promote(ctx context.Context, tx pgx.Tx, facilityID uuid.UUID, start, end time.Time) error
}

// WithPromotion attaches the waitlist to the cancel path. Optional: without it
// a cancellation is simply a cancellation, and every other guarantee holds.
func (s *Service) WithPromotion(p Promoter) *Service {
	s.promoter = p
	return s
}

// roleManager may act on any booking, not only their own.
const roleManager = "MANAGER"

// statusCancelled is the terminal state a cancel converges on.
const statusCancelled = "CANCELLED"

// errNoRowsCancelled is internal: the guarded UPDATE matched nothing. It never
// escapes Cancel, which resolves it into convergence or ErrNotCancellable.
var errNoRowsCancelled = errors.New("cancel matched no rows")

// Cancel releases a booking. IMPLEMENTATION.md §6.1.
//
// One transaction: authorise, cancel under a status guard, release shared
// capacity, record the event, enqueue the notification.
//
// THERE IS NO "RELEASE THE SLOT" STEP, and its absence is the point of
// non-negotiable #4. no_double_book's predicate covers CONFIRMED, HELD and
// BLOCKED only, so setting the status to CANCELLED drops the row out of the
// constraint's partial index and the slot is bookable again the moment this
// transaction commits. Availability is derived from the bookings table at read
// time, so there is no is_available flag to clear and nothing that could
// disagree with the row.
//
// Shared facilities are the exception, and only because Mechanism B keeps a
// counter rather than deriving from the rows: that counter is decremented here,
// in the same transaction, so it cannot drift from the booking it accounts for.
func (s *Service) Cancel(ctx context.Context, bookingID, actorID uuid.UUID, reason string) (*Booking, error) {
	if bookingID == uuid.Nil {
		return nil, invalid("booking_id", "required")
	}
	if actorID == uuid.Nil {
		return nil, invalid("actor_id", "required")
	}

	var cancelled Booking
	err := store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		// This load exists ONLY to shape the error: it separates 404 from 403,
		// which a single guarded UPDATE cannot do because both collapse into
		// "zero rows".
		//
		// The invariant that keeps it on the right side of the read-then-write
		// ban: NOTHING READ HERE FEEDS THE UPDATE'S DECISION. The guarded UPDATE
		// alone decides whether the cancel happens, so a load that goes stale
		// mid-race degrades to zero rows and ErrNotCancellable — a correct
		// answer, never an incorrect write.
		//
		// Do not "optimise" this by dropping the UPDATE's status guard because
		// the status was already checked here. That is precisely the
		// read-then-write bug, reintroduced.
		existing, err := loadBooking(ctx, tx, bookingID)
		if err != nil {
			return err
		}

		if err := s.authorise(ctx, tx, existing, actorID); err != nil {
			return err
		}

		// The guarded UPDATE is the concurrency control. Two simultaneous
		// cancels both run it; exactly one matches a row.
		row, fromStatus, err := cancelGuarded(ctx, tx, bookingID)
		if errors.Is(err, errNoRowsCancelled) {
			// This call did not perform the cancellation. That is not
			// automatically a failure — non-negotiable #5 requires a retry to
			// return the original result, and a cancel whose 200 was lost in
			// transit is exactly that case. Telling the student "that booking is
			// no longer active" for an action that succeeded is a scary error
			// for a correct outcome.
			//
			// So converge on state instead: if the booking is already
			// CANCELLED, the caller's intent is satisfied and the truthful
			// answer is the cancelled booking. Any other terminal state
			// (COMPLETED, NO_SHOW) genuinely is a conflict.
			//
			// The status is RE-READ rather than taken from the load above. By
			// the time the UPDATE matches zero rows the winner has committed —
			// had it still been open, the UPDATE would have blocked on its row
			// lock instead — so the load is stale by construction here.
			current, loadErr := loadBooking(ctx, tx, bookingID)
			if loadErr != nil {
				return loadErr
			}
			if current.Status != statusCancelled {
				return fmt.Errorf("%w: booking is %s", ErrNotCancellable, current.Status)
			}

			// Converged. NO SIDE EFFECTS on this path: the capacity release, the
			// event and the outbox row belong to the call that matched the row,
			// and repeating them here would return a place twice and append a
			// duplicate event.
			cancelled = *current
			cancelled.Converged = true
			return nil
		}
		if err != nil {
			return err
		}

		// Shared facilities give their place back here, inside the same
		// transaction as the status change, so a cancel that rolls back cannot
		// leave a place returned to a booking that still exists.
		if !row.isExclusive {
			f, err := s.catalogue.Get(ctx, row.facilityID)
			if err != nil {
				return fmt.Errorf("booking: cancel: catalogue: %w", err)
			}
			if _, err := ReleaseCapacity(ctx, tx, f, row.facilityID, row.start, row.end); err != nil {
				return err
			}
		}

		if err := insertBookingEvent(ctx, tx, bookingID, actorID, &fromStatus, "CANCELLED", reason); err != nil {
			return store.Classify(err)
		}

		// The freed window is offered to the head of the queue HERE, in this
		// transaction, so a promotion cannot outlive a cancel that rolled back
		// (§6.2). internal/waitlist runs it under a savepoint and guarantees this
		// transaction is still usable whatever it returns.
		//
		// The error is logged and dropped ON PURPOSE. A cancel must never fail
		// because a promotion failed: the student cancelling did nothing wrong,
		// their court is genuinely released either way, and the queue entry that
		// could not be promoted is still WAITING for the sweeper or the next
		// cancel to pick up. Turning somebody else's missed offer into this
		// student's error would be the wrong person paying.
		//
		// Exclusive facilities only. A HELD row reserves nothing on a shared
		// facility, whose occupancy is the slot_capacity counter rather than the
		// exclusion constraint, so an offer there would not hold the place it
		// promised. waitlist.Join refuses those queues for the same reason.
		if s.promoter != nil && row.isExclusive {
			if err := s.promoter.Promote(ctx, tx, row.facilityID, row.start, row.end); err != nil {
				slog.WarnContext(ctx, "waitlist promotion failed; cancellation stands",
					"err", err,
					"booking_id", bookingID,
					"facility_id", row.facilityID,
					"start", row.start)
			}
		}

		if _, err := outbox.Enqueue(ctx, tx, outbox.TopicBookingCancelled, map[string]any{
			"booking_id":  bookingID,
			"facility_id": row.facilityID,
			"user_id":     row.userID,
			"start":       row.start,
			"end":         row.end,
			"actor_id":    actorID,
			"reason":      reason,
		}); err != nil {
			return store.Classify(err)
		}

		cancelled = Booking{
			ID:         row.id,
			Reference:  Reference(row.id),
			FacilityID: row.facilityID,
			UserID:     row.userID,
			Start:      row.start,
			End:        row.end,
			Status:     "CANCELLED",
			CreatedAt:  row.createdAt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &cancelled, nil
}

// authorise permits the owner, or a manager acting on anyone's booking.
//
// The role lookup only runs when the actor is not the owner, which is the common
// case — a student cancelling their own booking costs no extra query.
func (s *Service) authorise(ctx context.Context, q store.Querier, b *Booking, actorID uuid.UUID) error {
	if b.UserID == actorID {
		return nil
	}

	var role string
	err := q.QueryRow(ctx, queries.Get(queries.UserRole), actorID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: unknown actor", ErrForbidden)
	}
	if err != nil {
		return fmt.Errorf("booking: authorise: %w", err)
	}

	if role != roleManager {
		return fmt.Errorf("%w: booking belongs to another user", ErrForbidden)
	}
	return nil
}

// cancelRow is the subset of the cancelled booking the caller needs.
type cancelRow struct {
	id          uuid.UUID
	facilityID  uuid.UUID
	userID      uuid.UUID
	isExclusive bool
	start       time.Time
	end         time.Time
	createdAt   time.Time
}

// cancelGuarded runs the status-guarded UPDATE. Zero rows means the booking was
// not in a cancellable state — already cancelled, or already completed.
func cancelGuarded(ctx context.Context, q store.Querier, bookingID uuid.UUID) (cancelRow, string, error) {
	var (
		row        cancelRow
		fromStatus string
	)

	err := q.QueryRow(ctx, queries.Get(queries.BookingCancel), bookingID).Scan(
		&row.id, &row.facilityID, &row.userID, &row.isExclusive,
		&row.start, &row.end, &row.createdAt, &fromStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deliberately NOT ErrNotCancellable. Zero rows means only "this call
		// did not perform the cancellation"; whether that is a conflict or a
		// satisfied retry depends on the booking's current status, which the
		// caller checks.
		return row, "", errNoRowsCancelled
	}
	if err != nil {
		return row, "", store.Classify(err)
	}
	return row, fromStatus, nil
}

// loadBooking reads the booking being acted on.
func loadBooking(ctx context.Context, q store.Querier, id uuid.UUID) (*Booking, error) {
	var (
		b      Booking
		status string
		key    *string
	)
	err := q.QueryRow(ctx, queries.Get(queries.BookingGet), id).Scan(
		&b.ID, &b.FacilityID, &b.UserID, &b.isExclusive,
		&b.Start, &b.End, &status, &key, &b.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: booking %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("booking: load: %w", err)
	}

	b.Status = status
	b.Reference = Reference(b.ID)
	if key != nil {
		b.IdemKey = *key
	}
	return &b, nil
}

// Bookings is a student's own list, split at the current time.
type Bookings struct {
	Upcoming []Booking
	Past     []Booking
}

// ListMine returns the caller's active bookings, upcoming first.
//
// Served from the replica: this is a read, and a second of replication lag on
// somebody's own booking list is harmless. The query matches
// idx_bookings_user_upcoming's predicate and sort order, so the index serves
// both the filter and the ordering.
func (s *Service) ListMine(ctx context.Context, userID uuid.UUID) (*Bookings, error) {
	if userID == uuid.Nil {
		return nil, invalid("user_id", "required")
	}

	rows, err := s.db.Replica.Query(ctx, queries.Get(queries.BookingListMine), userID)
	if err != nil {
		return nil, fmt.Errorf("booking: list: %w", err)
	}
	defer rows.Close()

	now := s.now()
	out := &Bookings{}

	for rows.Next() {
		var b Booking
		if err := rows.Scan(
			&b.ID, &b.FacilityID, &b.FacilityName, &b.UserID,
			&b.Start, &b.End, &b.Status, &b.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("booking: list: scan: %w", err)
		}
		b.Reference = Reference(b.ID)

		// A booking still running counts as upcoming: the student is at the
		// court right now and it is the row they need on screen.
		if b.End.After(now) {
			out.Upcoming = append(out.Upcoming, b)
		} else {
			out.Past = append(out.Past, b)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("booking: list: %w", err)
	}

	// Past reads most-recent-first; upcoming reads soonest-first. Both are what
	// the screen wants, and the query's ordering already gives ascending.
	reverse(out.Past)

	return out, nil
}

func reverse(bs []Booking) {
	for i, j := 0, len(bs)-1; i < j; i, j = i+1, j-1 {
		bs[i], bs[j] = bs[j], bs[i]
	}
}
