// Package booking is the write path. Everything else in this codebase exists to
// make Create correct under contention.
package booking

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// clockSlack is how far in the past a start time may be before it is rejected.
// A phone with a slightly wrong clock should not be told its booking is in the
// past (§4.2).
const clockSlack = 60 * time.Second

// Booking is a confirmed reservation.
type Booking struct {
	ID         uuid.UUID
	Reference  string
	FacilityID uuid.UUID
	// FacilityName is populated by list queries that join the catalogue; the
	// write path leaves it empty because the caller already knows the facility.
	FacilityName string
	UserID       uuid.UUID
	Start        time.Time
	End          time.Time
	Status       string
	IdemKey      string
	CreatedAt    time.Time

	// Replayed is true when this booking was returned for a repeated
	// Idempotency-Key rather than created by this request. The API returns 200
	// instead of 201 for these.
	Replayed bool

	// Converged is true when a cancel found the booking already cancelled and
	// returned it unchanged rather than doing the work again. The caller's
	// intent was already satisfied; no side effect ran on this path.
	Converged bool

	// isExclusive mirrors the row's denormalised flag, so cancel knows whether
	// to release a capacity counter without a second catalogue lookup.
	isExclusive bool
}

// CreateRequest is one booking attempt.
type CreateRequest struct {
	FacilityID uuid.UUID
	UserID     uuid.UUID
	Start      time.Time
	Duration   time.Duration

	// IdemKey is the client's per-submit-intention key. Optional at this layer;
	// the HTTP layer requires it.
	IdemKey string
}

// End is the exclusive end of the requested window.
func (r CreateRequest) End() time.Time { return r.Start.Add(r.Duration) }

// Catalogue is the facility lookup the service needs. An interface so tests can
// substitute one without a database, and so the cache lives in one place.
type Catalogue interface {
	Get(ctx context.Context, id uuid.UUID) (*facility.Facility, error)
}

// Service orchestrates the write path. It holds no booking state of its own —
// the database is the only authority on who won.
type Service struct {
	db        *store.DB
	catalogue Catalogue
	loc       *time.Location
	now       func() time.Time

	// alts turns a 409 into somewhere else to go (§5.3). Optional: nil means a
	// bare, still-correct, still-fast conflict. See WithAlternatives.
	alts *Alternatives

	// promoter offers a cancelled window to the waitlist, inside the cancelling
	// transaction (§6.2). Optional: nil means a cancel is a plain cancel, which
	// is exactly what it was before M4. See WithPromotion.
	promoter Promoter
}

// NewService wires the write path. loc is the campus timezone, used only to
// decide which local day a slot falls on; everything is stored UTC.
func NewService(db *store.DB, catalogue Catalogue, loc *time.Location) *Service {
	if loc == nil {
		loc = time.UTC
	}
	return &Service{db: db, catalogue: catalogue, loc: loc, now: time.Now}
}

// WithClock overrides the clock. Used by tests.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Create books a slot. IMPLEMENTATION.md §4.1.
//
//	1  validate, before any database work
//	2  BEGIN
//	3    branch on is_exclusive
//	4    INSERT booking          (Mechanism A)
//	5    INSERT booking_events
//	6    INSERT outbox
//	7  COMMIT
//
// There is no SELECT of slot occupancy anywhere in this function. Availability
// is never read before writing; the insert is attempted and the database decides.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Booking, error) {
	// ---- 1. Validation, before the transaction -----------------------------
	//
	// Cheap rejections happen here so the herd never reaches the GiST index. A
	// facility cache hit means a malformed request costs zero queries.
	f, err := s.catalogue.Get(ctx, req.FacilityID)
	if err != nil {
		if errors.Is(err, facility.ErrNotFound) {
			return nil, fmt.Errorf("%w: facility %s", ErrNotFound, req.FacilityID)
		}
		return nil, fmt.Errorf("booking: catalogue: %w", err)
	}

	if err := s.validate(f, req); err != nil {
		return nil, err
	}

	// ---- 2. Branch on the facility's mechanism -----------------------------
	//
	// onConflict decorates a lost race with alternatives and the waitlist flag
	// (§5.3). It runs AFTER the transaction has rolled back — that is the whole
	// reason it sits here and not inside either attempt — and it passes anything
	// that is not a conflict straight through.
	if !f.IsExclusive {
		b, err := s.createShared(ctx, f, req)
		return b, s.onConflict(ctx, f, req, err)
	}
	b, err := s.createExclusive(ctx, f, req)
	return b, s.onConflict(ctx, f, req, err)
}

// maxDeadlockAttempts bounds the retry described on attemptExclusive.
const maxDeadlockAttempts = 3

// createExclusive runs the write transaction, retrying ONLY on a deadlock.
//
// To be unambiguous about a rule this project takes seriously: a 23P01 is never
// retried. A conflict is a verdict — the caller lost the slot, and the loser must
// lose fast.
//
// A 40P01 is not a verdict. Postgres aborts an arbitrary transaction to break a
// lock cycle, and on the booking path those cycles are structural: two inserts
// for overlapping windows each place an index tuple before scanning, so each can
// wait on the other while checking the exclusion constraint. The victim wrote
// nothing and learned nothing about who owns the slot. Retrying re-asks the
// question; the answer is then a clean confirmation or a clean 23P01.
//
// Measured on this schema, an unretried burst of 500 leaves roughly a third of
// requests holding a raw 40P01 instead of a conflict, and takes minutes.
func (s *Service) createExclusive(ctx context.Context, f *facility.Facility, req CreateRequest) (*Booking, error) {
	var lastErr error
	for attempt := 0; attempt < maxDeadlockAttempts; attempt++ {
		b, err := s.attemptExclusive(ctx, f, req)
		if !errors.Is(err, store.ErrDeadlock) {
			return b, err
		}
		lastErr = err

		// Stagger retries so victims of the same cycle do not collide again.
		// Jitter comes from the user id, which is already distinct per caller —
		// no RNG, so the behaviour stays reproducible in tests.
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("booking: %w", ctx.Err())
		case <-time.After(backoff(attempt, req.UserID)):
		}
	}
	return nil, fmt.Errorf("booking: create: exhausted %d attempts: %w", maxDeadlockAttempts, lastErr)
}

// backoff returns a short, deterministic, per-caller delay.
func backoff(attempt int, user uuid.UUID) time.Duration {
	base := time.Duration(1<<attempt) * time.Millisecond
	jitter := time.Duration(user[0]) * 20 * time.Microsecond // 0-5ms
	return base + jitter
}

// createShared is Mechanism B: claim a place in each slot the booking spans,
// then write the booking. Both run in ONE transaction, so a booking can never
// exist without its capacity being accounted for, and a counter can never be
// incremented for a booking that rolled back.
//
// No advisory lock here, deliberately. The lock in front of Mechanism A exists
// because concurrent overlapping inserts deadlock inside the GiST exclusion
// constraint check. Shared rows carry is_exclusive = false and so are not in
// that index at all; capacity_take serialises on a btree row lock instead, which
// does not have the problem. Taking the facility lock here would cap the gym at
// one concurrent booker and defeat the point of having capacity.
func (s *Service) createShared(ctx context.Context, f *facility.Facility, req CreateRequest) (*Booking, error) {
	var lastErr error
	for attempt := 0; attempt < maxDeadlockAttempts; attempt++ {
		b, err := s.attemptShared(ctx, f, req)
		if !errors.Is(err, store.ErrDeadlock) {
			return b, err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("booking: %w", ctx.Err())
		case <-time.After(backoff(attempt, req.UserID)):
		}
	}
	return nil, fmt.Errorf("booking: create: exhausted %d attempts: %w", maxDeadlockAttempts, lastErr)
}

func (s *Service) attemptShared(ctx context.Context, f *facility.Facility, req CreateRequest) (*Booking, error) {
	var idemKey *string
	if req.IdemKey != "" {
		key := req.IdemKey
		idemKey = &key
	}

	start, end := req.Start.UTC(), req.End().UTC()
	slots := slotsFor(f, start, end)

	var created Booking
	txErr := store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		// One capacity_take per slot, in ascending slot_start order. Consistent
		// ordering across all callers is what keeps the multi-slot path
		// deadlock-free: two transactions claiming the same rows in opposite
		// orders would wait on each other.
		//
		// All of them are in this transaction, so a booking that spans two slots
		// and finds the second one full releases the first automatically on
		// rollback. There is no partial claim to clean up.
		for _, slot := range slots {
			if _, err := capacityTake(ctx, tx, f.ID, slot, f.Capacity); err != nil {
				if errors.Is(err, ErrCapacityFull) {
					return err
				}
				return store.Classify(err)
			}
		}

		id, createdAt, err := insertShared(ctx, tx, f.ID, req.UserID, start, end, idemKey)
		if err != nil {
			return store.Classify(err)
		}

		if err := insertBookingEvent(ctx, tx, id, req.UserID, nil, "CONFIRMED", "created"); err != nil {
			return store.Classify(err)
		}

		if _, err := outbox.Enqueue(ctx, tx, outbox.TopicBookingConfirmed, map[string]any{
			"booking_id":  id,
			"facility_id": f.ID,
			"user_id":     req.UserID,
			"start":       start,
			"end":         end,
			"slots":       len(slots),
		}); err != nil {
			return store.Classify(err)
		}

		created = Booking{
			ID:         id,
			Reference:  Reference(id),
			FacilityID: f.ID,
			UserID:     req.UserID,
			Start:      start,
			End:        end,
			Status:     "CONFIRMED",
			IdemKey:    req.IdemKey,
			CreatedAt:  createdAt,
		}
		return nil
	})

	if txErr == nil {
		return &created, nil
	}

	switch {
	case errors.Is(txErr, ErrCapacityFull):
		return nil, txErr

	case errors.Is(txErr, store.ErrDeadlock):
		return nil, txErr

	case errors.Is(txErr, store.ErrIdempotentReplay):
		// Transaction already rolled back by WithTx; fresh connection now.
		return s.findByIdemKey(ctx, req.UserID, req.IdemKey)

	case errors.Is(txErr, store.ErrTimeout):
		return nil, fmt.Errorf("booking: %w", context.DeadlineExceeded)

	default:
		return nil, fmt.Errorf("booking: create: %w", txErr)
	}
}

// Release gives back every place a shared booking held, in its own transaction.
//
// The in-transaction primitive is ReleaseCapacity; use that from cancel and the
// no-show sweep, which must release and change the booking status atomically.
// This wrapper exists for callers that have nothing else to do.
func (s *Service) Release(ctx context.Context, facilityID uuid.UUID, start, end time.Time) ([]Counter, error) {
	f, err := s.catalogue.Get(ctx, facilityID)
	if err != nil {
		return nil, fmt.Errorf("booking: release: %w", err)
	}

	var out []Counter
	err = store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		var err error
		out, err = ReleaseCapacity(ctx, tx, f, facilityID, start.UTC(), end.UTC())
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) attemptExclusive(ctx context.Context, f *facility.Facility, req CreateRequest) (*Booking, error) {
	var idemKey *string
	if req.IdemKey != "" {
		key := req.IdemKey
		idemKey = &key
	}

	start, end := req.Start.UTC(), req.End().UTC()

	var created Booking
	txErr := store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		// Queue behind any other writer for this facility. Not a correctness
		// mechanism — see lockFacility. Released by COMMIT or ROLLBACK.
		if err := lockFacility(ctx, tx, f.ID); err != nil {
			return store.Classify(err)
		}

		// Mechanism A. A plain INSERT — no occupancy read precedes it.
		id, createdAt, err := insertExclusive(ctx, tx, f.ID, req.UserID, start, end, idemKey)
		if err != nil {
			return store.Classify(err)
		}

		if err := insertBookingEvent(ctx, tx, id, req.UserID, nil, "CONFIRMED", "created"); err != nil {
			return store.Classify(err)
		}

		// Side effects go through the outbox, inside this transaction. The
		// trigger's pg_notify fires on commit, so a rolled-back booking can
		// never produce a notification.
		if _, err := outbox.Enqueue(ctx, tx, outbox.TopicBookingConfirmed, map[string]any{
			"booking_id":  id,
			"facility_id": f.ID,
			"user_id":     req.UserID,
			"start":       start,
			"end":         end,
		}); err != nil {
			return store.Classify(err)
		}

		created = Booking{
			ID:         id,
			Reference:  Reference(id),
			FacilityID: f.ID,
			UserID:     req.UserID,
			Start:      start,
			End:        end,
			Status:     "CONFIRMED",
			IdemKey:    req.IdemKey,
			CreatedAt:  createdAt,
		}
		return nil
	})

	if txErr == nil {
		return &created, nil
	}

	// ---- Error mapping -----------------------------------------------------
	//
	// By the time WithTx returns, the transaction has already been rolled back.
	// That matters: once a statement raises, the transaction is aborted and no
	// further query runs on that connection. Every lookup below therefore runs
	// on a FRESH connection from the pool (§4.5). Getting this wrong presents as
	// a flaky test; it is not flaky, it is the aborted-transaction rule.
	switch {
	case errors.Is(txErr, store.ErrDeadlock):
		// Surfaced to createExclusive, which decides whether to re-ask.
		return nil, txErr

	case errors.Is(txErr, store.ErrIdempotentReplay):
		// The unique index did the work. Return the original booking.
		return s.findByIdemKey(ctx, req.UserID, req.IdemKey)

	case errors.Is(txErr, store.ErrSlotTaken):
		// The user may have raced themselves: two concurrent submits with the
		// same key, where their own earlier attempt won the slot. In that case
		// the honest answer is the original booking, not a conflict against
		// itself. One indexed lookup on uq_bookings_user_idem, only when a key
		// was supplied.
		//
		// This is NOT a retry of the insert. The loser still loses; we are only
		// deciding which answer the loser deserves.
		if req.IdemKey != "" {
			if existing, err := s.findByIdemKey(ctx, req.UserID, req.IdemKey); err == nil {
				return existing, nil
			}
		}
		return nil, ErrSlotTaken

	case errors.Is(txErr, store.ErrTimeout):
		return nil, fmt.Errorf("booking: %w", context.DeadlineExceeded)

	default:
		return nil, fmt.Errorf("booking: create: %w", txErr)
	}
}

// findByIdemKey re-reads the booking a repeated Idempotency-Key already created.
//
// Runs on a fresh pooled connection, after the write transaction has rolled back.
func (s *Service) findByIdemKey(ctx context.Context, userID uuid.UUID, idemKey string) (*Booking, error) {
	if idemKey == "" {
		return nil, fmt.Errorf("%w: no idempotency key to replay", ErrNotFound)
	}

	var (
		b      Booking
		key    *string
		status string
	)
	err := s.db.Primary.QueryRow(ctx, queries.Get(queries.BookingFindByIdem), userID, idemKey).Scan(
		&b.ID, &b.FacilityID, &b.UserID, &b.Start, &b.End, &status, &key, &b.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: no booking for idempotency key", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("booking: replay lookup: %w", err)
	}

	b.Status = status
	b.Reference = Reference(b.ID)
	if key != nil {
		b.IdemKey = *key
	}
	b.Replayed = true
	return &b, nil
}

// insertBookingEvent appends to the audit trail. from is nil for a creation,
// which has no previous status.
func insertBookingEvent(ctx context.Context, q store.Querier, bookingID, actorID uuid.UUID, from *string, to, reason string) error {
	_, err := q.Exec(ctx, queries.Get(queries.BookingEventInsert), bookingID, actorID, from, to, reason)
	return err
}

// ---------------------------------------------------------------------------
// Validation — §4.2
// ---------------------------------------------------------------------------

// validate rejects everything that can be decided without the database.
//
// This runs before BEGIN on purpose: at 6 PM most requests are for the same
// contended slot, and a malformed one should never reach the GiST index at all.
func (s *Service) validate(f *facility.Facility, req CreateRequest) error {
	if req.UserID == uuid.Nil {
		return invalid("user_id", "required")
	}
	if !f.IsActive {
		return invalid("facility_id", "facility %s is not active", f.Name)
	}
	if req.Duration <= 0 {
		return invalid("duration", "must be positive")
	}
	if req.Duration < f.MinDuration {
		return invalid("duration", "minimum booking is %s", f.MinDuration)
	}
	if req.Duration > f.MaxDuration {
		return invalid("duration", "maximum booking is %s", f.MaxDuration)
	}

	// 60s of slack, so a phone with a slightly wrong clock still books.
	if req.Start.Before(s.now().Add(-clockSlack)) {
		return invalid("start", "slot is in the past")
	}

	// Opening hours are compared on the facility's LOCAL day. Storing UTC and
	// comparing UTC would put an 18:00 IST slot at 12:30 UTC and silently
	// reject it against a 06:00-22:00 window.
	local := req.Start.In(s.loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.loc)

	offsetStart := local.Sub(midnight)
	offsetEnd := offsetStart + req.Duration

	if offsetStart < f.OpensAt {
		return invalid("start", "%s opens at %s", f.Name, clock(f.OpensAt))
	}
	// End may equal ClosesAt: the window is half-open, so a booking ending
	// exactly at closing time does not extend past it.
	if offsetEnd > f.ClosesAt {
		return invalid("duration", "%s closes at %s", f.Name, clock(f.ClosesAt))
	}

	// Grid alignment applies to SHARED facilities only, and the asymmetry is
	// deliberate rather than an oversight.
	//
	// Mechanism B keeps one counter row per (facility, slot_start). That key
	// only means anything on a fixed grid: a booking starting at 18:30 has no
	// counter row to increment, and inventing one would let 18:00-19:00 and
	// 18:30-19:30 each claim a full capacity for overlapping time. So shared
	// facilities are grid-aligned BY CONSTRUCTION.
	//
	// Exclusive facilities keep full variable-duration freedom, because
	// Mechanism A protects a tstzrange with an exclusion constraint and needs no
	// grid at all. The grid must not leak back into Mechanism A: a slot-key
	// there would be silently incorrect the moment a duration varied.
	if !f.IsExclusive {
		if err := validateAlignment(f, req, offsetStart); err != nil {
			return err
		}
	}

	return nil
}

// CodeSlotNotAligned is the API error code for an off-grid shared booking.
const CodeSlotNotAligned = "SLOT_NOT_ALIGNED"

// validateAlignment requires the start to land on a granularity boundary and the
// duration to be a whole number of slots.
//
// Boundaries are measured from the facility's opening time, not from midnight. A
// venue opening at 05:30 with 60-minute slots has blocks at 05:30, 06:30, ... —
// anchoring to midnight would declare every one of them misaligned.
func validateAlignment(f *facility.Facility, req CreateRequest, offsetStart time.Duration) error {
	if f.Granularity <= 0 {
		return nil
	}

	if (offsetStart-f.OpensAt)%f.Granularity != 0 {
		return &ValidationError{
			Field:   "start",
			Code:    CodeSlotNotAligned,
			Message: fmt.Sprintf("%s books in %s blocks from %s", f.Name, f.Granularity, clock(f.OpensAt)),
		}
	}

	if req.Duration%f.Granularity != 0 {
		return &ValidationError{
			Field:   "duration",
			Code:    CodeSlotNotAligned,
			Message: fmt.Sprintf("%s books in whole %s blocks", f.Name, f.Granularity),
		}
	}

	return nil
}

// clock renders an offset from midnight as HH:MM, for error messages.
func clock(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}

// Reference is the short human-readable booking code shown on the confirmation
// screen and quoted at the venue desk. Derived from the id, so it needs no
// column and cannot drift.
func Reference(id uuid.UUID) string {
	return "PH-" + strings.ToUpper(id.String()[:8])
}
