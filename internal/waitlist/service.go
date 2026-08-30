// Package waitlist is the second concurrency proof.
//
// The write path (internal/booking) proves that exactly one of five hundred
// simultaneous bookers wins a slot. This package proves the mirror image: when
// three students cancel three slots in the same instant, three DIFFERENT people
// come off the queue, concurrently. One statement carries that —
// store/queries/waitlist_claim_head.sql — and everything here exists to run it
// in the right transaction. That file is also honest about the division of
// labour inside it: the row lock and the WAITING predicate are what keep the
// promotions distinct, and SKIP LOCKED is what stops the claimants queueing
// behind one another to get them.
//
// Two rules shape the whole package:
//
//  1. A promotion happens inside the CANCELLING transaction. A promotion that
//     committed separately could outlive a cancel that rolled back, leaving a
//     student holding a court whose original booking is still confirmed.
//
//  2. A cancel must never fail because a promotion failed. The promotion runs
//     under a SAVEPOINT, so a 23P01 on the hold rolls back the promotion alone
//     and the cancellation still commits. The student who cancelled has done
//     nothing wrong and must not be told their cancellation failed.
package waitlist

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// DefaultPromotionTTL is the claim window when PROMOTION_TTL_MIN is unset.
const DefaultPromotionTTL = 10 * time.Minute

// Domain errors. As in internal/booking, these are the package's own
// vocabulary: store says what the database did, this says what it means for a
// student, and httpx maps them to status codes without ever seeing a SQLSTATE.
var (
	// ErrAlreadyWaiting means the student already holds a live entry for this
	// facility and window. Decided by uq_waitlist_live, not by a prior SELECT.
	// Maps to 409 ALREADY_WAITING.
	ErrAlreadyWaiting = errors.New("already on the waitlist")

	// ErrNotFound means no such waitlist entry. Maps to 404.
	ErrNotFound = errors.New("waitlist entry not found")

	// ErrForbidden means the entry belongs to somebody else. Maps to 403.
	ErrForbidden = errors.New("waitlist entry belongs to another user")

	// ErrNotWaiting means the entry exists but is no longer queueing — it was
	// promoted, claimed, expired or already abandoned. Maps to 409.
	ErrNotWaiting = errors.New("waitlist entry is no longer waiting")

	// ErrValidation means the request was malformed. Maps to 422.
	ErrValidation = errors.New("validation failed")
)

// Entry is one place in a queue.
type Entry struct {
	ID         uuid.UUID
	FacilityID uuid.UUID
	UserID     uuid.UUID
	Start      time.Time
	End        time.Time
	Priority   int

	// Position is the bigserial ordering key from migration 0005. It is the
	// queue's order, not a student's place in it: the first person ever to queue
	// for badminton may hold position 87.
	Position int64

	// Place is what a student reads — 1 for the head of the queue. Derived at
	// read time from the entries actually still WAITING, so a queue that empties
	// ahead of you moves you up without anything being rewritten.
	Place int

	Status    string
	CreatedAt time.Time
}

// Promotion is one offer made off the queue.
type Promotion struct {
	EntryID      uuid.UUID
	UserID       uuid.UUID
	BookingID    uuid.UUID
	FacilityID   uuid.UUID
	Start        time.Time
	End          time.Time
	OfferExpires time.Time
}

// Catalogue is the facility lookup this package needs, declared here so the
// dependency points inward. facility.Repo satisfies it.
type Catalogue interface {
	Get(ctx context.Context, id uuid.UUID) (*facility.Facility, error)
}

// Service owns the queue and the promotion protocol.
type Service struct {
	db        *store.DB
	catalogue Catalogue
	ttl       time.Duration
	log       *slog.Logger
}

// NewService wires the waitlist. promotionTTL is PROMOTION_TTL_MIN: how long a
// promoted student has to claim their court before the sweeper reclaims it.
func NewService(db *store.DB, catalogue Catalogue, promotionTTL time.Duration, log *slog.Logger) *Service {
	if promotionTTL == 0 {
		promotionTTL = DefaultPromotionTTL
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, catalogue: catalogue, ttl: promotionTTL, log: log}
}

// ---------------------------------------------------------------------------
// Joining and leaving
// ---------------------------------------------------------------------------

// Join puts a student in the queue for a window and returns their place in it.
//
// The position is never computed here. It is the bigserial the INSERT returns,
// so two students joining in the same millisecond cannot be handed the same
// place — and a duplicate live entry is rejected by uq_waitlist_live rather
// than by a SELECT that checked first. The read-then-write ban is not special
// to the booking path.
func (s *Service) Join(ctx context.Context, userID, facilityID uuid.UUID, start, end time.Time) (*Entry, error) {
	if userID == uuid.Nil {
		return nil, invalid("user_id", "required")
	}
	if facilityID == uuid.Nil {
		return nil, invalid("facility_id", "required")
	}
	if !end.After(start) {
		return nil, invalid("duration", "must be positive")
	}

	f, err := s.catalogue.Get(ctx, facilityID)
	if err != nil {
		if errors.Is(err, facility.ErrNotFound) {
			return nil, fmt.Errorf("%w: facility %s", ErrNotFound, facilityID)
		}
		return nil, fmt.Errorf("waitlist: catalogue: %w", err)
	}
	if !f.IsActive {
		return nil, invalid("facility_id", "%s is not active", f.Name)
	}

	// Shared facilities are refused rather than silently queued. Promotion works
	// by inserting a HELD booking, and a HELD row reserves nothing on a facility
	// whose occupancy is the slot_capacity counter — the offer would not hold
	// the place it promised. A queue that can never promote is worse than no
	// queue, because the student stops looking for another slot.
	if !f.IsExclusive {
		return nil, invalid("facility_id", "%s books by capacity; there is no queue for it", f.Name)
	}

	entry := Entry{
		FacilityID: facilityID,
		UserID:     userID,
		Start:      start.UTC(),
		End:        end.UTC(),
		Status:     "WAITING",
	}

	// Priority tiers land with internal/policy; everyone queues in the default
	// tier for now and FIFO within it does the ordering.
	const defaultPriority = 0

	err = s.db.Primary.QueryRow(ctx, queries.Get(queries.WaitlistJoin),
		facilityID, userID, entry.Start, entry.End, defaultPriority,
	).Scan(&entry.ID, &entry.Position, &entry.Priority, &entry.CreatedAt)
	if err != nil {
		classified := store.Classify(err)
		if errors.Is(classified, store.ErrAlreadyWaiting) {
			return nil, fmt.Errorf("%w: %s", ErrAlreadyWaiting, f.Name)
		}
		return nil, fmt.Errorf("waitlist: join: %w", classified)
	}

	place, _, err := s.place(ctx, entry.ID)
	if err != nil {
		return nil, err
	}
	entry.Place = place
	return &entry, nil
}

// Leave abandons a queue entry. Only the owner may, and only while WAITING.
//
// A PROMOTED entry is deliberately refused: it owns a HELD booking that is
// reserving a court, and giving that court back is a cancellation of the
// booking (which re-runs the promotion machinery for the next student), not a
// quiet edit to a queue row.
func (s *Service) Leave(ctx context.Context, id, actorID uuid.UUID) error {
	if id == uuid.Nil {
		return invalid("id", "required")
	}
	if actorID == uuid.Nil {
		return invalid("actor_id", "required")
	}

	var (
		owner  uuid.UUID
		status string
		left   int
	)
	err := s.db.Primary.QueryRow(ctx, queries.Get(queries.WaitlistLeave), id, actorID).
		Scan(&owner, &status, &left)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("waitlist: leave: %w", err)
	}

	if left == 1 {
		return nil
	}

	// The UPDATE matched nothing. The pre-update snapshot says why.
	if owner != actorID {
		return ErrForbidden
	}
	if status == "CANCELLED" {
		// Converged: a retried leave whose first response was lost. The
		// student's intent is satisfied and saying so is the truth
		// (non-negotiable #5).
		return nil
	}
	return fmt.Errorf("%w: entry is %s", ErrNotWaiting, status)
}

// Position returns the entry's current place in its queue, 1 for the head.
//
// Zero means the entry is no longer WAITING. Derived every time from the
// entries still in the queue — there is no stored place to go stale when
// somebody ahead leaves.
func (s *Service) Position(ctx context.Context, id uuid.UUID) (int, error) {
	if id == uuid.Nil {
		return 0, invalid("id", "required")
	}
	place, _, err := s.place(ctx, id)
	return place, err
}

func (s *Service) place(ctx context.Context, id uuid.UUID) (int, string, error) {
	var (
		place  int
		status string
	)
	err := s.db.Primary.QueryRow(ctx, queries.Get(queries.WaitlistPlace), id).Scan(&place, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return 0, "", fmt.Errorf("waitlist: place: %w", err)
	}
	return place, status, nil
}

// ---------------------------------------------------------------------------
// Promotion — IMPLEMENTATION.md §6.2
// ---------------------------------------------------------------------------

// errNobodyWaiting is internal: the queue for this window is empty. It never
// escapes Promote, which reports it as "no promotion", not as a failure.
var errNobodyWaiting = errors.New("nobody waiting")

// Promote offers a freed window to the head of its queue, INSIDE THE CALLER'S
// TRANSACTION. It satisfies booking.Promoter.
//
// Contract, and it is the reason this signature takes a pgx.Tx rather than a
// pool:
//
//   - It runs in the cancelling transaction, so a promotion cannot outlive a
//     cancel that rolled back.
//   - It never leaves that transaction unusable. Everything it does happens
//     under a SAVEPOINT; on any failure the savepoint is rolled back and the
//     caller may carry on committing the cancel.
//   - Nobody waiting, and a window that was taken between the cancel and the
//     hold (23P01), are ORDINARY OUTCOMES and return nil. They are not
//     failures, and the caller should not treat them as any.
func (s *Service) Promote(ctx context.Context, tx pgx.Tx, facilityID uuid.UUID, start, end time.Time) error {
	_, err := s.promote(ctx, tx, facilityID, start, end)
	return err
}

// promote is Promote with the outcome the sweeper wants to count.
func (s *Service) promote(ctx context.Context, tx pgx.Tx, facilityID uuid.UUID, start, end time.Time) (*Promotion, error) {
	// pgx implements a nested transaction as a SAVEPOINT, which is exactly what
	// §6.2 prescribes: the promotion gets its own unit of failure inside the
	// cancel's.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("waitlist: savepoint: %w", err)
	}

	promotion, err := s.offerHead(ctx, sp, facilityID, start, end)
	if err != nil {
		// ROLLBACK TO SAVEPOINT, which also releases the row lock
		// waitlist_claim_head took — so the entry we could not promote goes back
		// to the queue for the next cancel to claim, rather than being stranded.
		if rbErr := sp.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			// The savepoint would not roll back, so the caller's transaction is
			// genuinely poisoned and its commit will fail. Say so rather than
			// pretending the cancel is fine.
			return nil, fmt.Errorf("waitlist: promotion rollback failed, transaction is unusable: %w", rbErr)
		}

		switch {
		case errors.Is(err, errNobodyWaiting):
			// An empty queue is the common case. A cancel is then a plain cancel.
			return nil, nil

		case errors.Is(err, store.ErrSlotTaken):
			// Another cancel's promotion, or a booking that slipped in, already
			// covers this window. The exclusion constraint said so, which is the
			// only authority that could. The entry stays WAITING.
			s.log.InfoContext(ctx, "waitlist promotion lost the freed window",
				"facility_id", facilityID, "start", start)
			return nil, nil
		}
		return nil, err
	}

	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return nil, fmt.Errorf("waitlist: release savepoint: %w", err)
	}
	return promotion, nil
}

// offerHead is the four steps of §6.2, all on the savepoint.
func (s *Service) offerHead(ctx context.Context, q pgx.Tx, facilityID uuid.UUID, start, end time.Time) (*Promotion, error) {
	start, end = start.UTC(), end.UTC()

	// 1. Claim the head of the queue. SKIP LOCKED is what makes concurrent
	//    cancellations promote different students; see the .sql file.
	p := Promotion{FacilityID: facilityID, Start: start, End: end}
	var (
		priority int
		position int64
	)
	err := q.QueryRow(ctx, queries.Get(queries.WaitlistClaimHead), facilityID, start, end).
		Scan(&p.EntryID, &p.UserID, &priority, &position)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNobodyWaiting
	}
	if err != nil {
		return nil, fmt.Errorf("waitlist: claim head: %w", store.Classify(err))
	}

	// 2. Reserve the court. The offer is a real HELD booking inside
	//    no_double_book's predicate, so it holds the slot for the claim window.
	//    This can raise 23P01, and that is a normal outcome — see promote.
	var createdAt time.Time
	err = q.QueryRow(ctx, queries.Get(queries.BookingInsertHeld),
		facilityID, p.UserID, start, end, s.ttl.Seconds(),
	).Scan(&p.BookingID, &p.OfferExpires, &createdAt)
	if err != nil {
		return nil, store.Classify(err)
	}

	// 3. Point the queue entry at its offer.
	var (
		entryID uuid.UUID
		newPos  int64
	)
	if err := q.QueryRow(ctx, queries.Get(queries.WaitlistPromote),
		p.EntryID, p.BookingID, p.OfferExpires).Scan(&entryID, &newPos); err != nil {
		return nil, fmt.Errorf("waitlist: mark promoted: %w", store.Classify(err))
	}

	// The audit trail records the hold in the same transaction that created it.
	// from_status is a typed nil: a hold is a new row, so there is no previous
	// status to record.
	var noPreviousStatus *string
	if _, err := q.Exec(ctx, queries.Get(queries.BookingEventInsert),
		p.BookingID, p.UserID, noPreviousStatus, "HELD", "waitlist promotion"); err != nil {
		return nil, fmt.Errorf("waitlist: promotion event: %w", store.Classify(err))
	}

	// 4. The offer's side effect, on THIS savepoint and therefore inside the
	//    cancelling transaction. A promotion that loses the window to 23P01
	//    rolls the savepoint back, taking this row with it, so a student is
	//    never told about a court they were not actually offered —
	//    non-negotiable #7. Nothing sends from here; a worker does, after the
	//    commit.
	//
	//    The offer expiry travels in the payload because this notification is
	//    the only thing that makes the claim window usable: a promotion nobody
	//    hears about expires unclaimed and the queue moves for nothing.
	if err := outbox.Enqueue(ctx, q, outbox.TopicWaitlistPromoted, map[string]any{
		"entry_id":      p.EntryID,
		"booking_id":    p.BookingID,
		"facility_id":   facilityID,
		"user_id":       p.UserID,
		"start":         start,
		"end":           end,
		"offer_expires": p.OfferExpires,
	}); err != nil {
		return nil, store.Classify(err)
	}

	s.log.InfoContext(ctx, "waitlist promotion offered",
		"entry_id", p.EntryID, "user_id", p.UserID, "booking_id", p.BookingID,
		"facility_id", facilityID, "start", start, "offer_expires", p.OfferExpires)

	return &p, nil
}

// ---------------------------------------------------------------------------

// ValidationError names the field that failed, mirroring booking's.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// Unwrap makes errors.Is(err, ErrValidation) true for every validation failure.
func (e *ValidationError) Unwrap() error { return ErrValidation }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}
