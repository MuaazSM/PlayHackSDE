package checkin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// Windows.
const (
	// DefaultEarlyWindow is how long before a slot starts a student may check
	// in. Ten minutes, per §7: long enough to walk in and scan, short enough
	// that "checked in" still means "at the venue, about to play".
	DefaultEarlyWindow = 10 * time.Minute

	// DefaultGracePeriod is GRACE_PERIOD_MIN's default: how long after the start
	// a court is held for somebody who has not arrived.
	DefaultGracePeriod = 15 * time.Minute

	// SweepInterval is how often no-shows are collected (§7).
	//
	// A minute, against the waitlist sweeper's thirty seconds, because the cost
	// of the delay is different: an unclaimed promotion offer is holding a court
	// that somebody has explicitly queued for, while a no-show is holding one
	// that has already been sitting empty for GRACE_PERIOD_MIN. Another thirty
	// seconds on top of fifteen minutes is not worth a transaction every second.
	SweepInterval = time.Minute
)

// sweepBatch bounds one pass, for the same reason the waitlist sweeper bounds
// its own: a pass holds row locks for its duration, and an unbounded batch after
// an outage would be one long transaction blocking live writes. Whatever is left
// over is picked up a minute later.
const sweepBatch = 100

// Domain errors. This package's vocabulary, mapped to status codes in httpx and
// nowhere else.
//
// Ownership and existence deliberately REUSE booking's sentinels rather than
// declaring near-identical twins: a 404 for a booking is the same 404 whichever
// endpoint asked, and two spellings of it would eventually be mapped two ways.
var (
	// ErrInvalidToken means the scanned code is not a live token for this
	// booking's facility — wrong venue, forged, or more than two windows old.
	// Maps to 403.
	ErrInvalidToken = errors.New("check-in token is not valid for this facility")

	// ErrOutsideWindow means the booking exists and belongs to the caller, but
	// now() is outside [start - early, start + grace]. Maps to 409.
	ErrOutsideWindow = errors.New("outside the check-in window")

	// ErrNotCheckable means the booking is not in a state that can be attended:
	// an unclaimed hold, or a booking already cancelled, completed, or released
	// as a no-show. Maps to 409.
	ErrNotCheckable = errors.New("booking cannot be checked in")
)

// Catalogue is the facility lookup this package needs, declared here so the
// dependency points inward. facility.Repo satisfies it.
type Catalogue interface {
	Get(ctx context.Context, id uuid.UUID) (*facility.Facility, error)
}

// Attendance is a recorded check-in.
type Attendance struct {
	BookingID  uuid.UUID
	Reference  string
	FacilityID uuid.UUID
	UserID     uuid.UUID
	Start      time.Time
	End        time.Time
	At         time.Time
	Method     string

	// Converged is true when the student was already checked in and this call
	// returned the original record rather than writing a second one. Both calls
	// answer 200; nothing happened twice.
	Converged bool
}

// Service owns redemption and the no-show sweep.
type Service struct {
	db        *store.DB
	catalogue Catalogue
	minter    *Minter
	early     time.Duration
	grace     time.Duration
	log       *slog.Logger

	// promoter offers a released window to the waitlist, inside the sweeping
	// transaction. It is booking.Promoter — the SAME interface the cancel path
	// takes, satisfied by the SAME waitlist.Service — so a no-show and a
	// cancellation reach the queue through one implementation of promotion.
	// Optional: nil means a no-show is simply a release.
	promoter booking.Promoter
}

// NewService wires check-in. grace is GRACE_PERIOD_MIN.
func NewService(db *store.DB, catalogue Catalogue, minter *Minter, grace time.Duration, log *slog.Logger) *Service {
	if grace <= 0 {
		grace = DefaultGracePeriod
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		db:        db,
		catalogue: catalogue,
		minter:    minter,
		early:     DefaultEarlyWindow,
		grace:     grace,
		log:       log,
	}
}

// WithPromotion attaches the waitlist to the no-show path.
//
// Pass the SAME waitlist.Service the cancel path uses. Two services over one
// database would still claim through waitlist_claim_head and so would still be
// correct, but they would hold different claim windows and the demo would be
// explaining two configurations of one feature.
func (s *Service) WithPromotion(p booking.Promoter) *Service {
	s.promoter = p
	return s
}

// WithWindow overrides [start - early, start + grace].
//
// Used by tests, which mint deliberately impossible windows — a NEGATIVE grace
// puts the deadline before the start, so a booking in the future is already past
// it — rather than sleeping through a real one. The check itself lives in
// Postgres' now(), so a test that moved a Go clock would prove nothing.
func (s *Service) WithWindow(early, grace time.Duration) *Service {
	s.early, s.grace = early, grace
	return s
}

// Minter exposes the token authority, for the venue display endpoint.
func (s *Service) Minter() *Minter { return s.minter }

// ---------------------------------------------------------------------------
// Redemption — §7
// ---------------------------------------------------------------------------

// Redeem records attendance. IMPLEMENTATION.md §7.
//
// One transaction: load the booking (to learn whose it is and which venue's code
// should be on the wall), verify the token, verify the owner, then insert
// check_ins under a window guard.
//
// The token is verified against the booking's OWN facility. That is the whole
// security property: a student can only produce a valid token by being in front
// of the display at the venue they booked, within the last two minutes. A code
// photographed at the tennis courts does not check anybody into the gym, and a
// code photographed yesterday does not check anybody into anything.
func (s *Service) Redeem(ctx context.Context, bookingID, actorID uuid.UUID, token string) (*Attendance, error) {
	if bookingID == uuid.Nil {
		return nil, invalid("booking_id", "required")
	}
	if actorID == uuid.Nil {
		return nil, invalid("actor_id", "required")
	}
	if token == "" {
		return nil, invalid("token", "required")
	}

	var out Attendance
	err := store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		// This load exists ONLY to shape the answer: it separates 404 from 403,
		// and it supplies the facility the token must be for. NOTHING READ HERE
		// DECIDES WHETHER THE CHECK-IN HAPPENS — the guarded insert does, and a
		// load that went stale mid-flight degrades to zero rows and a conflict,
		// never to a wrong write. Same discipline as booking.Cancel's load.
		row, err := loadBooking(ctx, tx, bookingID)
		if err != nil {
			return err
		}

		if !s.minter.Verify(token, row.facilityID) {
			return fmt.Errorf("%w: %s", ErrInvalidToken, row.facilityID)
		}

		// Owner only, deliberately stricter than Cancel. Attendance is a claim
		// about a person being somewhere; a manager asserting it on a student's
		// behalf would make the no-show numbers describe nothing.
		if row.userID != actorID {
			return fmt.Errorf("%w: booking belongs to another user", booking.ErrForbidden)
		}

		at, method, inserted, err := s.insertCheckIn(ctx, tx, bookingID, token)
		if err != nil {
			return err
		}

		if !inserted {
			// Zero rows. Either the student is already checked in — a satisfied
			// retry, non-negotiable #5 — or the window is closed. The PRIMARY KEY
			// on check_ins tells the two apart; nothing here guesses.
			existing, found, err := existingCheckIn(ctx, tx, bookingID)
			if err != nil {
				return err
			}
			if found {
				out = Attendance{
					BookingID: bookingID, Reference: booking.Reference(bookingID),
					FacilityID: row.facilityID, UserID: row.userID,
					Start: row.start, End: row.end,
					At: existing.at, Method: existing.method, Converged: true,
				}
				return nil
			}

			// Re-read the status rather than trusting the load above: by the time
			// the guard matched zero rows, whatever changed it has committed.
			current, err := loadBooking(ctx, tx, bookingID)
			if err != nil {
				return err
			}
			if current.status != statusConfirmed {
				return fmt.Errorf("%w: booking is %s", ErrNotCheckable, current.status)
			}
			return fmt.Errorf("%w: %s opens at %s and closes at %s",
				ErrOutsideWindow, booking.Reference(bookingID),
				current.start.Add(-s.early).Format(time.RFC3339),
				current.start.Add(s.grace).Format(time.RFC3339))
		}

		// No outbox row, and that is not an omission. A check-in changes no
		// occupancy — the court was and remains booked by the same student — so
		// there is nothing for the grid to patch and nobody to notify. The one
		// side effect it has is negative: it removes this booking from the
		// no-show sweep's NOT EXISTS, which needs no message to anybody.
		out = Attendance{
			BookingID: bookingID, Reference: booking.Reference(bookingID),
			FacilityID: row.facilityID, UserID: row.userID,
			Start: row.start, End: row.end,
			At: at, Method: method,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "check-in recorded",
		"booking_id", out.BookingID, "user_id", out.UserID,
		"facility_id", out.FacilityID, "converged", out.Converged)
	return &out, nil
}

// insertCheckIn runs the window-guarded insert. inserted is false when it
// matched nothing, which the caller resolves rather than treating as a failure.
func (s *Service) insertCheckIn(ctx context.Context, q store.Querier, bookingID uuid.UUID, token string) (at time.Time, method string, inserted bool, err error) {
	var id uuid.UUID
	err = q.QueryRow(ctx, queries.Get(queries.CheckinRedeem),
		bookingID, methodQR, tokenID(token), s.early.Seconds(), s.grace.Seconds(),
	).Scan(&id, &at, &method)

	if errors.Is(err, pgx.ErrNoRows) {
		return at, "", false, nil
	}
	if err != nil {
		return at, "", false, fmt.Errorf("checkin: redeem: %w", store.Classify(err))
	}
	return at, method, true, nil
}

type checkInRow struct {
	at     time.Time
	method string
}

func existingCheckIn(ctx context.Context, q store.Querier, bookingID uuid.UUID) (checkInRow, bool, error) {
	var (
		row checkInRow
		id  uuid.UUID
	)
	err := q.QueryRow(ctx, queries.Get(queries.CheckinGet), bookingID).
		Scan(&id, &row.at, &row.method)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, fmt.Errorf("checkin: load attendance: %w", store.Classify(err))
	}
	return row, true, nil
}

// methodQR is the only redemption route today. The column exists so a desk
// override ("MANUAL") can be added without a migration.
const methodQR = "QR"

// tokenID is what the audit trail keeps: a short prefix of the presented token,
// never the token itself.
//
// The token is a keyed hash of the minute, so storing it whole would be storing
// a credential that is already worthless two minutes later — all cost, no value.
// A prefix is enough to correlate two scans of the same displayed code while
// being useless to anybody who reads the table.
func tokenID(token string) string {
	const n = 12
	if len(token) <= n {
		return token
	}
	return token[:n]
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

const statusConfirmed = "CONFIRMED"

type bookingRow struct {
	id          uuid.UUID
	facilityID  uuid.UUID
	userID      uuid.UUID
	isExclusive bool
	start       time.Time
	end         time.Time
	status      string
}

// loadBooking reads the row being acted on, through the shared booking_get
// statement rather than a second spelling of it.
func loadBooking(ctx context.Context, q store.Querier, id uuid.UUID) (*bookingRow, error) {
	var (
		row     bookingRow
		idemKey *string
		created time.Time
	)
	err := q.QueryRow(ctx, queries.Get(queries.BookingGet), id).Scan(
		&row.id, &row.facilityID, &row.userID, &row.isExclusive,
		&row.start, &row.end, &row.status, &idemKey, &created,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: booking %s", booking.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("checkin: load booking: %w", store.Classify(err))
	}
	return &row, nil
}

// ---------------------------------------------------------------------------
// The sweep — §7
// ---------------------------------------------------------------------------

// SweepResult is what one pass did.
type SweepResult struct {
	// Completed is the number of attended bookings retired at their slot end.
	Completed int
	// NoShows is the number of courts released because nobody arrived.
	NoShows int

	// PromotionsAttempted is how many released windows were handed to the
	// promotion path.
	//
	// Named for what it actually counts. booking.Promoter reports only an error,
	// and waitlist treats an empty queue and a window lost to 23P01 as ORDINARY
	// outcomes returning nil — correctly, because neither is a failure. So a
	// window nobody was queueing for is counted here too, and calling this
	// "Promoted" would quietly overstate M-7 in exactly the direction that
	// flatters the demo. The database is the authority on who was promoted;
	// this is the sweeper's own bookkeeping.
	PromotionsAttempted int
}

// Typed nils and constants for the audit trail, as in the waitlist sweeper: an
// untyped nil would leave the driver guessing at an enum and a uuid column.
var (
	noActor             *uuid.UUID
	statusConfirmedFrom = statusConfirmed
)

// noShow is one released court.
type noShow struct {
	bookingID   uuid.UUID
	facilityID  uuid.UUID
	userID      uuid.UUID
	isExclusive bool
	start       time.Time
	end         time.Time
}

// completed is one attended booking whose window has closed.
type completed struct {
	bookingID  uuid.UUID
	facilityID uuid.UUID
	userID     uuid.UUID
	start      time.Time
	end        time.Time
}

// Sweep runs one pass: attended bookings whose windows have closed go to
// COMPLETED, unattended ones past the grace period go to NO_SHOW, and every
// released window is offered to the head of its queue.
//
// THE PROMOTION IS waitlist.Service.Promote — the same method a live cancel and
// the offer-expiry sweeper call, claiming through the same waitlist_claim_head
// statement. That is the point of choosing check-in as the second innovation:
// the machinery already existed and the incremental cost is this loop. Growing a
// second "who is next" query here would put two independent readers on the
// WAITING rows, and the failure that produces — one student handed two courts —
// is exactly the class of bug this project is judged on.
//
// One transaction per pass, so a release and the promotion it triggers commit
// together. There is never an instant where a court has been freed and nobody
// has been offered it.
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult

	err := store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		done, err := s.completeAttended(ctx, tx)
		if err != nil {
			return err
		}
		res.Completed = len(done)

		for _, c := range done {
			if err := s.event(ctx, tx, c.bookingID, "COMPLETED", "slot ended after check-in"); err != nil {
				return err
			}
		}

		absent, err := s.releaseNoShows(ctx, tx)
		if err != nil {
			return err
		}
		res.NoShows = len(absent)

		for _, n := range absent {
			if err := s.event(ctx, tx, n.bookingID, "NO_SHOW", "no check-in within the grace period"); err != nil {
				return err
			}

			// Shared facilities give the place back explicitly, because their
			// occupancy is the slot_capacity counter rather than the exclusion
			// constraint. In THIS transaction, so a sweep that rolls back cannot
			// leave a place returned for a booking still standing.
			if !n.isExclusive {
				f, err := s.catalogue.Get(ctx, n.facilityID)
				if err != nil {
					return fmt.Errorf("checkin: sweep: catalogue: %w", err)
				}
				if _, err := booking.ReleaseCapacity(ctx, tx, f, n.facilityID, n.start, n.end); err != nil {
					return err
				}
			}

			// Enqueued BEFORE the promotion below, and the order matters for the
			// same reason it does in the waitlist sweeper: outbox rows drain by
			// (created_at, id), every row in one transaction shares a created_at
			// because now() is transaction time, so the id is the tiebreaker.
			// Inserting here first is what makes a released window publish
			// free-then-held rather than leaving every grid showing a slot that
			// is not actually free.
			if err := outbox.Enqueue(ctx, tx, outbox.TopicBookingNoShow, map[string]any{
				"booking_id":  n.bookingID,
				"facility_id": n.facilityID,
				"user_id":     n.userID,
				"start":       n.start,
				"end":         n.end,
			}); err != nil {
				return store.Classify(err)
			}

			// Exclusive facilities only, exactly as on the cancel path: a HELD row
			// reserves nothing on a shared facility, so an offer there would not
			// hold the place it promised. waitlist.Join refuses those queues for
			// the same reason.
			if s.promoter != nil && n.isExclusive {
				if err := s.promoter.Promote(ctx, tx, n.facilityID, n.start, n.end); err != nil {
					return fmt.Errorf("checkin: sweep: promote: %w", err)
				}
				res.PromotionsAttempted++
			}
		}
		return nil
	})
	if err != nil {
		return SweepResult{}, err
	}

	if res.NoShows > 0 || res.Completed > 0 {
		s.log.InfoContext(ctx, "no-show sweep",
			"completed", res.Completed, "no_shows", res.NoShows,
			"promotions_attempted", res.PromotionsAttempted)
	}
	return res, nil
}

// releaseNoShows runs the batch UPDATE and drains the result set completely
// before returning.
//
// Draining first is not tidiness: pgx runs one query at a time per connection,
// and the caller issues more statements on this transaction for every row here.
// Holding the rows open while doing that would fail on the first of them.
func (s *Service) releaseNoShows(ctx context.Context, tx pgx.Tx) ([]noShow, error) {
	rows, err := tx.Query(ctx, queries.Get(queries.BookingMarkNoShow), s.grace.Seconds(), sweepBatch)
	if err != nil {
		return nil, fmt.Errorf("checkin: mark no-shows: %w", store.Classify(err))
	}
	defer rows.Close()

	var out []noShow
	for rows.Next() {
		var n noShow
		if err := rows.Scan(&n.bookingID, &n.facilityID, &n.userID, &n.isExclusive, &n.start, &n.end); err != nil {
			return nil, fmt.Errorf("checkin: mark no-shows: scan: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checkin: mark no-shows: %w", store.Classify(err))
	}
	return out, nil
}

// completeAttended retires the bookings that were used, draining for the same
// reason releaseNoShows does.
func (s *Service) completeAttended(ctx context.Context, tx pgx.Tx) ([]completed, error) {
	rows, err := tx.Query(ctx, queries.Get(queries.BookingCompleteAttended), sweepBatch)
	if err != nil {
		return nil, fmt.Errorf("checkin: complete attended: %w", store.Classify(err))
	}
	defer rows.Close()

	var out []completed
	for rows.Next() {
		var c completed
		if err := rows.Scan(&c.bookingID, &c.facilityID, &c.userID, &c.start, &c.end); err != nil {
			return nil, fmt.Errorf("checkin: complete attended: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checkin: complete attended: %w", store.Classify(err))
	}
	return out, nil
}

// event appends to the audit trail. actor is NULL: nobody did this, a clock did.
func (s *Service) event(ctx context.Context, tx pgx.Tx, bookingID uuid.UUID, to, reason string) error {
	if _, err := tx.Exec(ctx, queries.Get(queries.BookingEventInsert),
		bookingID, noActor, &statusConfirmedFrom, to, reason); err != nil {
		return fmt.Errorf("checkin: %s event: %w", to, store.Classify(err))
	}
	return nil
}

// RunSweeper sweeps every interval until ctx is cancelled.
//
// A failed pass is logged and the next one is scheduled. There is nothing to
// recover: the whole pass is one transaction, so a failure committed nothing and
// the next tick finds exactly the same work. Stopping the loop on an error would
// turn a transient database blip into courts that stay locked up all evening.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = SweepInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.log.Info("no-show sweeper started", "interval", interval, "grace", s.grace)

	for {
		select {
		case <-ctx.Done():
			s.log.Info("no-show sweeper stopped")
			return
		case <-ticker.C:
			if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("no-show sweep failed; retrying next tick", "err", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------

// ValidationError names the field that failed, mirroring booking's and
// waitlist's so httpx maps one shape.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// Unwrap makes errors.Is(err, booking.ErrValidation) true for every validation
// failure here, so the existing 422 mapping covers this package without a second
// sentinel that means the same thing.
func (e *ValidationError) Unwrap() error { return booking.ErrValidation }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}
