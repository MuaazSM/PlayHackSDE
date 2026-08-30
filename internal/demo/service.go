package demo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxN bounds a race. Peak on this campus is 300-500 concurrent requests in the
// seconds after evening slots open (CLAUDE.md, "Scale, honestly"), so 5000 is an
// order of magnitude of headroom over anything worth demonstrating and still
// small enough that a mistyped n cannot spawn a million goroutines against the
// booking path.
const MaxN = 5000

// DefaultDuration is the slot length a race books when the caller does not say.
const DefaultDuration = time.Hour

var (
	// ErrNoBookers means the users table has no students to race as. The fix is
	// `make seed`, and saying so beats a confusing empty result on stage.
	ErrNoBookers = errors.New("demo: no seeded students to race as — run `make seed`")

	// ErrInvalid means the race request was malformed. Maps to 422.
	ErrInvalid = errors.New("demo: invalid request")
)

// Catalogue is the facility lookup the demo needs. Same interface the booking
// service takes, satisfied by *facility.Repo.
type Catalogue = booking.Catalogue

// Service runs the race console's two operations against one local database.
//
// It holds the booking service and calls Create on it DIRECTLY. See Run for why
// that is the whole design, not a shortcut.
type Service struct {
	db        *store.DB
	bookings  *booking.Service
	catalogue Catalogue
}

// NewService wires the race console over the same booking service the API uses.
//
// Deliberately the same instance: a demo that ran against a specially configured
// service would prove something about that configuration rather than about the
// system a student books through.
func NewService(db *store.DB, bookings *booking.Service, catalogue Catalogue) *Service {
	return &Service{db: db, bookings: bookings, catalogue: catalogue}
}

// Request is one race: n attempts at one facility and one slot.
type Request struct {
	FacilityID uuid.UUID
	Start      time.Time
	Duration   time.Duration // zero means DefaultDuration
	N          int
}

// End is the exclusive end of the contended window.
func (r Request) End() time.Time { return r.Start.Add(r.duration()) }

func (r Request) duration() time.Duration {
	if r.Duration <= 0 {
		return DefaultDuration
	}
	return r.Duration
}

// Winner names whoever actually holds the court, read back from the database.
type Winner struct {
	BookingID uuid.UUID `json:"booking_id"`
	Reference string    `json:"reference"`
	User      string    `json:"user"`
}

// Result is the race console's payload, IMPLEMENTATION.md §13.
//
// DBCount is the proof. Everything else is telemetry: it describes what the
// process running the race observed, and a process can be wrong about itself.
// DBCount is the database's own answer, asked fresh after every goroutine
// finished, and it is the number that goes on screen in large type.
type Result struct {
	N           int `json:"n"`
	Confirmed   int `json:"confirmed"`
	Conflict409 int `json:"conflict_409"`
	Other       int `json:"other"`
	DBCount     int `json:"db_count"`

	ElapsedMS int64 `json:"elapsed_ms"`
	P50MS     int64 `json:"p50_ms"`
	P99MS     int64 `json:"p99_ms"`

	// RejectP99MS is the M-3 number on its own: rejections target p99 < 150 ms.
	// At peak most users lose, so the loser path is the majority experience and
	// gets its own line rather than being averaged into the total.
	RejectP99MS int64 `json:"reject_p99_ms"`

	// StartSpreadMS is the gap between the first and last goroutine entering the
	// booking service. It answers the only honest objection to this whole demo —
	// "were they really simultaneous?" — with a number instead of an assurance.
	StartSpreadMS int64 `json:"start_spread_ms"`

	Winner *Winner `json:"winner,omitempty"`

	// Errors samples anything that was neither a confirmation nor a clean
	// conflict. A run where 499 requests fail with an unclassified error also
	// yields db_count == 1 and is NOT a pass; surfacing the cause here stops a
	// defect hiding inside the loser count.
	Errors []string `json:"errors,omitempty"`
}

// maxReportedErrors caps the Errors sample. Three is enough to diagnose and
// short enough that a broken run does not produce a megabyte of JSON.
const maxReportedErrors = 3

// Run fires N concurrent booking attempts at one slot and reads the answer back
// out of the database. IMPLEMENTATION.md §13.
//
// # It calls the domain service directly, in-process
//
// Not over HTTP. A judge may well ask why, and the answer is that the mechanism
// under test is the exclusion constraint and nothing else should be in the
// frame. Going over the loopback would put an HTTP server, a JSON round trip,
// the IP and per-user rate limiters, the idempotency middleware and the load
// shedder between the goroutine and the INSERT. Every one of those is real and
// tested elsewhere (test/api, test/httpx) — but here they would distort the
// result in the direction of flattering us: the shedder would reject most of the
// burst with a fast 429 before it ever reached the GiST index, so the demo would
// be showing off admission control while claiming to show off a database
// constraint.
//
// So: no network, no rate limiter, no shedder. Every one of the N attempts
// reaches the write path and takes its answer from Postgres.
//
// # The barrier
//
// All N goroutines block on one channel that is closed after every one of them
// has parked, so they genuinely contend rather than trickle. This is the same
// Race helper the concurrency suite runs (test/testutil.Race wraps it), which is
// the point: the demo on stage and the test in CI exercise one code path.
func (s *Service) Run(ctx context.Context, req Request) (*Result, error) {
	if req.N <= 0 {
		return nil, fmt.Errorf("%w: n must be at least 1", ErrInvalid)
	}
	if req.N > MaxN {
		return nil, fmt.Errorf("%w: n must be at most %d", ErrInvalid, MaxN)
	}
	if req.FacilityID == uuid.Nil {
		return nil, fmt.Errorf("%w: facility_id is required", ErrInvalid)
	}
	if req.Start.IsZero() {
		return nil, fmt.Errorf("%w: start is required", ErrInvalid)
	}

	start, end := req.Start.UTC(), req.End().UTC()
	duration := req.duration()

	// Fail before the race rather than after it, so a misconfigured demo says
	// what is wrong instead of reporting 500 identical errors.
	if _, err := s.catalogue.Get(ctx, req.FacilityID); err != nil {
		if errors.Is(err, facility.ErrNotFound) {
			return nil, fmt.Errorf("%w: facility %s", ErrInvalid, req.FacilityID)
		}
		return nil, fmt.Errorf("demo: catalogue: %w", err)
	}

	bookers, err := s.bookers(ctx, req.N)
	if err != nil {
		return nil, err
	}

	// Open connections up front. Without this the first run measures connection
	// setup, and a demo whose first fire is slower than its second invites
	// exactly the wrong question.
	s.warm(ctx, req.N)

	out, err := Race(ctx, req.N, func(ctx context.Context, i int) (any, error) {
		return s.bookings.Create(ctx, booking.CreateRequest{
			FacilityID: req.FacilityID,
			UserID:     bookers[i%len(bookers)].id,
			Start:      start,
			Duration:   duration,

			// A fresh key per attempt, exactly as a real client generates one per
			// submit intention. Distinct keys are what keep uq_bookings_user_idem
			// out of the result: every rejection below comes from the exclusion
			// constraint, not from the idempotency index noticing a repeat.
			IdemKey: uuid.NewString(),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("demo: race: %w", err)
	}

	res := &Result{
		N:             req.N,
		Confirmed:     len(out.Successes()),
		ElapsedMS:     millis(out.Elapsed),
		P50MS:         millis(Percentile(out.Attempts, 50)),
		P99MS:         millis(Percentile(out.Attempts, 99)),
		RejectP99MS:   millis(Percentile(out.Failures(), 99)),
		StartSpreadMS: millis(out.StartSpread),
	}

	// A conflict is either mechanism saying no: the exclusion constraint for an
	// exclusive court, the capacity counter for the gym. Both are a 409 to the
	// user, so both belong in the same bucket. Anything else is a defect.
	for _, a := range out.Failures() {
		switch {
		case errors.Is(a.Err, booking.ErrSlotTaken), errors.Is(a.Err, booking.ErrCapacityFull):
			res.Conflict409++
		default:
			res.Other++
			if len(res.Errors) < maxReportedErrors {
				res.Errors = append(res.Errors, a.Err.Error())
			}
		}
	}

	// The proof. A FRESH query, on the primary, after every goroutine finished.
	res.DBCount, err = s.Count(ctx, req.FacilityID, start, end)
	if err != nil {
		return nil, err
	}

	res.Winner, err = s.winner(ctx, req.FacilityID, start, end)
	if err != nil {
		return nil, err
	}

	return res, nil
}

// ResetResult reports what a reset cleared.
type ResetResult struct {
	FacilityID uuid.UUID `json:"facility_id"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Cancelled  int       `json:"cancelled"`

	// DBCount is read back the same way Run reads it, and after a reset it is
	// zero. Same number, same query, same proof — the console can show the slot
	// going to 0 and back to 1 without ever changing which question it asks.
	DBCount int `json:"db_count"`
}

// Reset clears the demo slot so the race is re-runnable, live, on stage, twice.
//
// One transaction: cancel every live booking overlapping the window, give back
// any shared capacity those bookings held, and record an event for each so the
// audit trail says what happened.
//
// It does NOT go through booking.Cancel, deliberately. A student cancelling
// their court is a real event that promotes the head of the waitlist and
// notifies people; a demo reset is stage management, and must do neither. If it
// promoted, a waitlisted student would take the slot half a second before the
// presenter fired the race again, and the next run would produce zero winners.
func (s *Service) Reset(ctx context.Context, facilityID uuid.UUID, start, end time.Time) (*ResetResult, error) {
	if facilityID == uuid.Nil {
		return nil, fmt.Errorf("%w: facility_id is required", ErrInvalid)
	}
	if start.IsZero() {
		return nil, fmt.Errorf("%w: start is required", ErrInvalid)
	}
	if !end.After(start) {
		return nil, fmt.Errorf("%w: end must be after start", ErrInvalid)
	}

	f, err := s.catalogue.Get(ctx, facilityID)
	if err != nil {
		if errors.Is(err, facility.ErrNotFound) {
			return nil, fmt.Errorf("%w: facility %s", ErrInvalid, facilityID)
		}
		return nil, fmt.Errorf("demo: catalogue: %w", err)
	}

	startUTC, endUTC := start.UTC(), end.UTC()
	res := &ResetResult{FacilityID: facilityID, Start: startUTC, End: endUTC}

	err = store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		cleared, err := cancelSlot(ctx, tx, facilityID, startUTC, endUTC)
		if err != nil {
			return err
		}

		for _, c := range cleared {
			// Shared facilities keep a counter rather than deriving occupancy
			// from the rows, so their places are given back here, in the same
			// transaction as the status change. Exclusive facilities need no
			// such step — that is non-negotiable #4 doing its job.
			if !c.isExclusive {
				if _, err := booking.ReleaseCapacity(ctx, tx, f, facilityID, c.start, c.end); err != nil {
					return err
				}
			}

			from := c.fromStatus
			if _, err := tx.Exec(ctx, queries.Get(queries.BookingEventInsert),
				c.id, nil, &from, statusCancelled, "demo reset"); err != nil {
				return store.Classify(err)
			}
		}

		res.Cancelled = len(cleared)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("demo: reset: %w", err)
	}

	// Read back after the commit, on a fresh connection. Zero means the stage is
	// clear; anything else means something wrote into the slot between the
	// commit and this query, which the presenter would rather know about.
	res.DBCount, err = s.Count(ctx, facilityID, startUTC, endUTC)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// statusCancelled is the terminal state a reset converges on.
const statusCancelled = "CANCELLED"

// clearedBooking is one row a reset cancelled.
type clearedBooking struct {
	id          uuid.UUID
	isExclusive bool
	start, end  time.Time
	fromStatus  string
}

// cancelSlot runs the guarded UPDATE and returns what it cleared.
func cancelSlot(ctx context.Context, tx pgx.Tx, facilityID uuid.UUID, start, end time.Time) ([]clearedBooking, error) {
	rows, err := tx.Query(ctx, queries.Get(queries.DemoResetSlot), facilityID, start, end)
	if err != nil {
		return nil, store.Classify(err)
	}
	defer rows.Close()

	var out []clearedBooking
	for rows.Next() {
		var c clearedBooking
		if err := rows.Scan(&c.id, &c.isExclusive, &c.start, &c.end, &c.fromStatus); err != nil {
			return nil, fmt.Errorf("demo: reset: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, store.Classify(err)
	}
	return out, nil
}

// Count is the proof query, exposed on its own so the console can re-ask it
// without re-running a race — "still 1" is a claim worth being able to check at
// any moment, including several minutes after the fact.
//
// Primary, never the replica: replication lag would let this report a stale
// count, and a proof that can lag is not a proof.
func (s *Service) Count(ctx context.Context, facilityID uuid.UUID, start, end time.Time) (int, error) {
	var n int
	err := s.db.Primary.QueryRow(ctx, queries.Get(queries.DemoCountConfirmed),
		facilityID, start.UTC(), end.UTC()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("demo: count confirmed: %w", err)
	}
	return n, nil
}

// winner reads back whoever holds the court. Nil when nobody does.
func (s *Service) winner(ctx context.Context, facilityID uuid.UUID, start, end time.Time) (*Winner, error) {
	var (
		id     uuid.UUID
		userID uuid.UUID
		roll   string
	)
	err := s.db.Primary.QueryRow(ctx, queries.Get(queries.DemoWinner),
		facilityID, start, end).Scan(&id, &userID, &roll)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("demo: winner: %w", err)
	}
	return &Winner{BookingID: id, Reference: booking.Reference(id), User: roll}, nil
}

// booker is one account the race fires as.
type booker struct {
	id   uuid.UUID
	roll string
}

// bookers reads the pool of students to race as.
//
// It reads rather than creates. The race console must leave the database as it
// found it apart from the one booking it is demonstrating, so it races as
// whoever `make seed` put there and never invents accounts mid-demo.
//
// Fewer students than attempts is fine and is the normal case: ten seeded
// students firing fifty attempts each still produces one winner, because every
// attempt carries its own idempotency key and so every rejection comes from the
// exclusion constraint. The CI gate (test/concurrency.TestConcurrentBooking_SingleWinner)
// runs the strict version with 500 distinct users, which is where that claim is
// actually proven.
func (s *Service) bookers(ctx context.Context, n int) ([]booker, error) {
	rows, err := s.db.Primary.Query(ctx, queries.Get(queries.DemoBookers), n)
	if err != nil {
		return nil, fmt.Errorf("demo: bookers: %w", err)
	}
	defer rows.Close()

	var out []booker
	for rows.Next() {
		var b booker
		if err := rows.Scan(&b.id, &b.roll); err != nil {
			return nil, fmt.Errorf("demo: bookers: scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("demo: bookers: %w", err)
	}
	if len(out) == 0 {
		return nil, ErrNoBookers
	}
	return out, nil
}

// warm opens connections before the race so the first fire measures contention
// rather than connection setup.
//
// n is clamped to the pool's own maximum: asking for more would block on an
// acquire that cannot succeed while this function still holds the others.
// Failures are ignored on purpose — warming is an optimisation, and a demo that
// refused to run because it could not pre-open a connection would be trading the
// deliverable for the nicety.
func (s *Service) warm(ctx context.Context, n int) {
	if max := int(s.db.Primary.Config().MaxConns); n > max {
		n = max
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conns := make([]*pgxpool.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := s.db.Primary.Acquire(ctx)
		if err != nil {
			break
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}

func millis(d time.Duration) int64 { return d.Milliseconds() }
