package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Manager closures. IMPLEMENTATION.md §10.4.
//
// A CLOSURE IS A BOOKING ROW: user_id NULL, status BLOCKED. There is no closures
// table and there should not be one — the constraint that stops two students
// sharing a court is then, unchanged and unextended, the same constraint that
// stops a student booking a closed one. Maintenance, a match, a monsoon: one
// mechanism, two features.
//
// That is the whole story for an EXCLUSIVE facility. For a SHARED one it is only
// half, and the missing half is the easiest thing in this project to get silently
// wrong. no_double_book's predicate is scoped to `is_exclusive`, because that is
// what lets Mechanism B exist at all (§3.2) — so a BLOCKED row on the gymnasium
// is not in the constraint's index, blocks nothing, and leaves a facility that
// reads 'closed' on the grid while still accepting bookings. Zeroing
// slot_capacity is the partner step, and it is not optional; see
// closure_zero_capacity.sql and TestClosure_BlocksBookingOnGym.
//
// Existing bookings inside a closed window are FLAGGED, never auto-cancelled. A
// human decides whether to revoke somebody's court, and does it through the
// ordinary cancel path so the student is notified, the counter is released and
// the queue is promoted.
// ---------------------------------------------------------------------------

// ClosureRequest is one manager's decision to take a window off the board.
type ClosureRequest struct {
	FacilityID uuid.UUID
	// ActorID is the manager. Recorded on the audit event; the role check itself
	// happens at the edge (httpx.RequireRole), where every other role check is.
	ActorID uuid.UUID
	Start   time.Time
	End     time.Time
	Reason  string
}

// AffectedBooking is a student's booking that sits inside a closed window.
//
// It carries the roll number and name because the point of the list is that
// somebody has to contact these people; an id would send staff back to the
// database to find out who they are.
type AffectedBooking struct {
	ID     uuid.UUID
	UserID uuid.UUID
	RollNo string
	Name   string
	Start  time.Time
	End    time.Time
	Status string
}

// Closure is a blocked window.
type Closure struct {
	ID           uuid.UUID
	FacilityID   uuid.UUID
	FacilityName string
	IsExclusive  bool
	Start        time.Time
	End          time.Time
	Reason       string
	Status       string
	CreatedAt    time.Time

	// ActorID is the manager who created the closure, from the audit event. Only
	// populated by ListClosures.
	ActorID uuid.UUID

	// Affected are the bookings a human still has to deal with. Always empty for
	// an exclusive facility on the success path — the exclusion constraint would
	// have rejected the closure instead.
	Affected []AffectedBooking

	// Slots counts the capacity counters this call zeroed (on create) or restored
	// (on reopen). Zero for exclusive facilities, which have no counters.
	Slots int

	// Replayed is true when an identical closure already existed and this call
	// returned it rather than creating a second one.
	Replayed bool

	// Converged is true when a reopen found the closure already withdrawn. The
	// caller's intent was already satisfied and no side effect ran.
	Converged bool
}

// ClosureFilter narrows the manager console's list. Both fields are optional.
type ClosureFilter struct {
	FacilityID uuid.UUID
	// Date is YYYY-MM-DD in the campus timezone, or empty for every day.
	Date string
}

// ClosureConflict is a closure refused because bookings already occupy the
// window.
//
// It unwraps to ErrSlotTaken, so the edge maps it to 409 through the same
// classify switch as a lost race — a closure that loses to an existing booking
// lost to exactly the same constraint, and pretending it is a different kind of
// failure would be inventing a second vocabulary for one mechanism.
//
// What it adds is the list. A manager told "conflict" with no names cannot act;
// a manager told which four bookings are in the way can ring four people.
type ClosureConflict struct {
	FacilityName string
	Bookings     []AffectedBooking
}

func (c *ClosureConflict) Error() string {
	return fmt.Sprintf("%s: %d booking(s) already inside that window", c.FacilityName, len(c.Bookings))
}

// Unwrap makes errors.Is(err, ErrSlotTaken) true.
func (c *ClosureConflict) Unwrap() error { return ErrSlotTaken }

// CreateClosure blocks a window. IMPLEMENTATION.md §10.4.
//
// One transaction:
//
//	1  pg_advisory_xact_lock(facility)     — first statement, as on the write path
//	2  find an identical closure           — retry convergence, §10.4/#5
//	3  INSERT bookings … status BLOCKED    — Mechanism A rejects it on conflict
//	4  UPSERT slot_capacity capacity = 0   — SHARED only, and this is the step
//	                                         that makes a gym closure real
//	5  read the bookings a human must resolve
//	6  INSERT booking_events, INSERT outbox
//
// The advisory lock is the same contention shaper the booking path takes, keyed
// the same way, and it does the same job here: an exclusive facility's closure
// and a burst of bookings for the same window are inserters into one GiST index,
// which is precisely the situation that deadlocks inside the constraint check
// rather than resolving cleanly. It decides nothing — with the lock removed a
// closure racing a booking still yields exactly one winner, chosen by
// no_double_book.
func (s *Service) CreateClosure(ctx context.Context, req ClosureRequest) (*Closure, error) {
	f, err := s.catalogue.Get(ctx, req.FacilityID)
	if err != nil {
		if errors.Is(err, facility.ErrNotFound) {
			return nil, fmt.Errorf("%w: facility %s", ErrNotFound, req.FacilityID)
		}
		return nil, fmt.Errorf("booking: closure: catalogue: %w", err)
	}
	if err := validateClosure(req); err != nil {
		return nil, err
	}

	start, end := req.Start.UTC(), req.End.UTC()

	out := Closure{
		FacilityID:   f.ID,
		FacilityName: f.Name,
		IsExclusive:  f.IsExclusive,
		Start:        start,
		End:          end,
		Reason:       req.Reason,
		Status:       statusBlocked,
	}

	txErr := store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		if err := lockFacility(ctx, tx, f.ID); err != nil {
			return store.Classify(err)
		}

		// A replayed request returns the original closure and runs no side
		// effect twice — non-negotiable #5. Safe against a concurrent duplicate
		// because the advisory lock above already serialised us; see
		// closure_find.sql for why the idempotency index cannot do this job.
		existing, createdAt, found, err := findClosure(ctx, tx, f.ID, start, end)
		if err != nil {
			return err
		}
		if found {
			out.ID, out.CreatedAt, out.Replayed = existing, createdAt, true
			out.Affected, err = affectedBookings(ctx, tx, f.ID, start, end)
			return err
		}

		id, createdAt, err := insertClosure(ctx, tx, f.ID, f.IsExclusive, start, end)
		if err != nil {
			return store.Classify(err)
		}
		out.ID, out.CreatedAt = id, createdAt

		// ---- THE SHARED-FACILITY STEP (§10.4 step 2) -----------------------
		//
		// Without this a gym closure blocks nothing at all. Skipping it for an
		// exclusive facility is not an optimisation: exclusive facilities have no
		// counter rows, and creating some would invent a second, unread account
		// of occupancy for a mechanism that derives it from the rows.
		if !f.IsExclusive {
			slots := closureSlots(f, s.loc, start, end)
			for _, slot := range slots {
				if err := zeroCapacity(ctx, tx, f.ID, slot); err != nil {
					return store.Classify(err)
				}
			}
			out.Slots = len(slots)
		}

		// Read, do not revoke (§10.4 step 3).
		out.Affected, err = affectedBookings(ctx, tx, f.ID, start, end)
		if err != nil {
			return err
		}

		// The reason lives here rather than on the booking row: booking_events is
		// already the audit trail, and a reason column would be a second place to
		// look for the same fact.
		if err := insertBookingEvent(ctx, tx, id, req.ActorID, nil, statusBlocked, req.Reason); err != nil {
			return store.Classify(err)
		}

		// Side effects go through the outbox, inside this transaction
		// (non-negotiable #7). facility_id and start are what live.StateFor needs
		// to patch the grid to 'closed'; the rest is for the notification.
		if err := outbox.Enqueue(ctx, tx, outbox.TopicClosureCreated, map[string]any{
			"closure_id":  id,
			"facility_id": f.ID,
			"start":       start,
			"end":         end,
			"reason":      req.Reason,
			"actor_id":    req.ActorID,
			"affected":    affectedIDs(out.Affected),
		}); err != nil {
			return store.Classify(err)
		}
		return nil
	})

	if txErr == nil {
		return &out, nil
	}

	// ---- Error mapping ----------------------------------------------------
	//
	// The transaction has already rolled back by the time WithTx returns, so the
	// conflict lookup below runs on a FRESH connection (§4.5). It goes to the
	// primary rather than the replica: this list is what staff act on, and a
	// replica that is half a second behind could omit the booking that just won
	// the window.
	switch {
	case errors.Is(txErr, store.ErrSlotTaken):
		conflict := &ClosureConflict{FacilityName: f.Name}
		if rows, err := affectedBookings(ctx, s.db.Primary, f.ID, start, end); err == nil {
			conflict.Bookings = rows
		}
		return nil, conflict

	case errors.Is(txErr, store.ErrTimeout):
		return nil, fmt.Errorf("booking: closure: %w", context.DeadlineExceeded)

	default:
		return nil, fmt.Errorf("booking: closure: %w", txErr)
	}
}

// Reopen withdraws a closure: the BLOCKED row is cancelled and, for a shared
// facility, every counter the closure zeroed is restored to the facility's
// declared capacity.
//
// The status-guarded UPDATE is the concurrency control, exactly as in Cancel:
// two simultaneous reopens both run it, one matches, and the capacity
// restoration, the audit event and the outbox row hang off the matched row so
// they fire exactly once.
func (s *Service) Reopen(ctx context.Context, closureID, actorID uuid.UUID, reason string) (*Closure, error) {
	if closureID == uuid.Nil {
		return nil, invalid("closure_id", "required")
	}
	if actorID == uuid.Nil {
		return nil, invalid("actor_id", "required")
	}

	var out Closure
	err := store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		current, err := loadClosure(ctx, tx, closureID)
		if err != nil {
			return err
		}

		// Queue behind any writer for this facility, for the same reason the
		// booking path does. Not the concurrency control — the guarded UPDATE
		// below is.
		if err := lockFacility(ctx, tx, current.FacilityID); err != nil {
			return store.Classify(err)
		}

		out = *current
		out.Reason = current.Reason
		out.Status = statusCancelled

		row, err := reopenGuarded(ctx, tx, closureID)
		if errors.Is(err, errNoRowsCancelled) {
			// Already withdrawn. A closure row is only ever BLOCKED or CANCELLED,
			// so zero rows can only mean somebody got here first — the caller's
			// intent is satisfied and the honest answer is the closure, not an
			// error. NO SIDE EFFECTS on this path: the restoration, the event and
			// the outbox row belong to the call that matched the row.
			out.Converged = true
			return nil
		}
		if err != nil {
			return err
		}

		f, err := s.catalogue.Get(ctx, row.facilityID)
		if err != nil {
			return fmt.Errorf("booking: reopen: catalogue: %w", err)
		}
		out.FacilityName = f.Name

		if !f.IsExclusive {
			// Restore in the same transaction as the status change, so a rollback
			// cannot leave the gym open with the closure still standing. The
			// statement's own guards skip slots another closure still covers.
			restored := 0
			for _, slot := range closureSlots(f, s.loc, row.start, row.end) {
				ok, err := restoreCapacity(ctx, tx, f.ID, slot, f.Capacity)
				if err != nil {
					return store.Classify(err)
				}
				if ok {
					restored++
				}
			}
			out.Slots = restored
		}

		from := statusBlocked
		if err := insertBookingEvent(ctx, tx, closureID, actorID, &from, statusCancelled, reason); err != nil {
			return store.Classify(err)
		}

		// booking.cancelled rather than a topic of its own: this releases a
		// window, which is what that topic means, and live.StateFor already maps
		// it to 'free' and invalidates the campus grid.
		if err := outbox.Enqueue(ctx, tx, outbox.TopicBookingCancelled, map[string]any{
			"booking_id":  closureID,
			"facility_id": row.facilityID,
			"start":       row.start,
			"end":         row.end,
			"actor_id":    actorID,
			"reason":      reason,
			"closure":     true,
		}); err != nil {
			return store.Classify(err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListClosures serves the manager console.
//
// Served from the REPLICA: it is a read, and a closure that is a second old on
// an admin screen costs nothing. The write path never consults it.
func (s *Service) ListClosures(ctx context.Context, filter ClosureFilter) ([]Closure, error) {
	var (
		facilityID *uuid.UUID
		from, to   *time.Time
	)
	if filter.FacilityID != uuid.Nil {
		id := filter.FacilityID
		facilityID = &id
	}
	if filter.Date != "" {
		// Localised HERE, not in SQL. The campus day is an IST day; resolving it
		// in the database would mean passing a zone name and trusting Postgres'
		// abbreviation table, where 'IST' is Israel.
		day, err := time.ParseInLocation("2006-01-02", filter.Date, s.loc)
		if err != nil {
			return nil, invalid("date", "must be YYYY-MM-DD")
		}
		start := day.UTC()
		end := day.AddDate(0, 0, 1).UTC()
		from, to = &start, &end
	}

	rows, err := s.db.Replica.Query(ctx, queries.Get(queries.ClosureList), facilityID, from, to)
	if err != nil {
		return nil, fmt.Errorf("booking: closures: %w", err)
	}
	defer rows.Close()

	out := []Closure{}
	for rows.Next() {
		var (
			c       Closure
			reason  *string
			actorID *uuid.UUID
		)
		if err := rows.Scan(&c.ID, &c.FacilityID, &c.FacilityName, &c.IsExclusive,
			&c.Start, &c.End, &c.CreatedAt, &reason, &actorID); err != nil {
			return nil, fmt.Errorf("booking: closures: scan: %w", err)
		}
		if reason != nil {
			c.Reason = *reason
		}
		if actorID != nil {
			c.ActorID = *actorID
		}
		c.Status = statusBlocked
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("booking: closures: %w", err)
	}
	return out, nil
}

// statusBlocked is the status a closure row holds while it stands.
const statusBlocked = "BLOCKED"

// validateClosure rejects what can be decided without the database.
//
// Deliberately NOT checked: whether the window is in the future, and whether it
// falls inside opening hours. A manager closing the rest of today's evening is
// the normal case, and a closure that spills past closing time is harmless — it
// covers slots that do not exist.
func validateClosure(req ClosureRequest) error {
	if req.ActorID == uuid.Nil {
		return invalid("actor_id", "required")
	}
	if req.Start.IsZero() || req.End.IsZero() {
		return invalid("start", "start and end are required")
	}
	if !req.End.After(req.Start) {
		return invalid("end", "must be after start")
	}
	return nil
}

// closureSlots enumerates the facility's grid blocks that OVERLAP [start, end).
//
// Not slotsFor. That helper steps from the booking's own start, which is correct
// for a shared BOOKING because the write path has already validated it onto the
// grid (422 SLOT_NOT_ALIGNED). A closure is not validated that way and must not
// be: a manager who closes 18:15-19:45 for a burst pipe means the 18:00 and 19:00
// blocks are gone, and snapping to their own start would create counter rows at
// 18:15 and 19:15 that nothing ever reads — the gym would stay bookable while the
// grid showed it closed. That is the exact silent failure §10.4 warns about,
// arrived at from the other direction.
//
// Blocks are anchored on the facility's opens_at in the CAMPUS timezone, the same
// anchor validateAlignment and the availability grid use, and are returned in
// ascending order so this walks the counter rows in the same order capacity_take
// does — which is what keeps the two deadlock-free against each other.
func closureSlots(f *facility.Facility, loc *time.Location, start, end time.Time) []Slot {
	if f.Granularity <= 0 {
		return []Slot{{Start: start, End: end}}
	}
	if loc == nil {
		loc = time.UTC
	}

	// From the local day BEFORE the window: a facility whose hours run past
	// midnight has blocks belonging to the previous day inside this window.
	first := localMidnight(start.In(loc).AddDate(0, 0, -1), loc)
	last := localMidnight(end.In(loc), loc)

	var out []Slot
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		for off := f.OpensAt; off+f.Granularity <= f.ClosesAt; off += f.Granularity {
			slotStart := day.Add(off)
			slotEnd := slotStart.Add(f.Granularity)

			// Half-open overlap, matching '[)' everywhere else: a block that ends
			// exactly when the closure starts is not inside it.
			if slotStart.Before(end) && slotEnd.After(start) {
				out = append(out, Slot{Start: slotStart.UTC(), End: slotEnd.UTC()})
			}
		}
	}
	return out
}

func localMidnight(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// ---------------------------------------------------------------------------
// Statement wrappers. One per .sql file, so a call site reads as the thing it
// does rather than as a query name.

func insertClosure(
	ctx context.Context,
	q store.Querier,
	facilityID uuid.UUID,
	isExclusive bool,
	start, end time.Time,
) (id uuid.UUID, createdAt time.Time, err error) {
	err = q.QueryRow(ctx, queries.Get(queries.ClosureInsert),
		facilityID, isExclusive, start, end,
	).Scan(&id, &createdAt)
	return id, createdAt, err
}

func findClosure(
	ctx context.Context,
	q store.Querier,
	facilityID uuid.UUID,
	start, end time.Time,
) (id uuid.UUID, createdAt time.Time, found bool, err error) {
	err = q.QueryRow(ctx, queries.Get(queries.ClosureFind), facilityID, start, end).
		Scan(&id, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, createdAt, false, nil
	}
	if err != nil {
		return id, createdAt, false, fmt.Errorf("booking: closure lookup: %w", err)
	}
	return id, createdAt, true, nil
}

func zeroCapacity(ctx context.Context, q store.Querier, facilityID uuid.UUID, slot Slot) error {
	var (
		at             time.Time
		booked, capped int
	)
	return q.QueryRow(ctx, queries.Get(queries.ClosureZeroCapacity),
		facilityID, slot.Start, slot.End).Scan(&at, &booked, &capped)
}

// restoreCapacity gives one slot its capacity back. false means the statement
// matched nothing — the slot was never closed, or another closure still covers
// it — which is a correct outcome, not an error.
func restoreCapacity(ctx context.Context, q store.Querier, facilityID uuid.UUID, slot Slot, capacity int) (bool, error) {
	var (
		at             time.Time
		booked, capped int
	)
	err := q.QueryRow(ctx, queries.Get(queries.ClosureRestoreCapacity),
		facilityID, slot.Start, capacity).Scan(&at, &booked, &capped)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func affectedBookings(ctx context.Context, q store.Querier, facilityID uuid.UUID, start, end time.Time) ([]AffectedBooking, error) {
	rows, err := q.Query(ctx, queries.Get(queries.ClosureAffected), facilityID, start, end)
	if err != nil {
		return nil, fmt.Errorf("booking: closure: affected bookings: %w", err)
	}
	defer rows.Close()

	out := []AffectedBooking{}
	for rows.Next() {
		var a AffectedBooking
		if err := rows.Scan(&a.ID, &a.UserID, &a.RollNo, &a.Name, &a.Start, &a.End, &a.Status); err != nil {
			return nil, fmt.Errorf("booking: closure: affected bookings: scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("booking: closure: affected bookings: %w", err)
	}
	return out, nil
}

func affectedIDs(rows []AffectedBooking) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// closureRow is the subset of the withdrawn closure the reopen path needs.
type closureRow struct {
	id          uuid.UUID
	facilityID  uuid.UUID
	isExclusive bool
	start       time.Time
	end         time.Time
	createdAt   time.Time
}

// reopenGuarded runs the status-guarded UPDATE. Zero rows means this call did not
// perform the reopen; the caller decides what that means.
func reopenGuarded(ctx context.Context, q store.Querier, id uuid.UUID) (closureRow, error) {
	var row closureRow
	err := q.QueryRow(ctx, queries.Get(queries.ClosureReopen), id).Scan(
		&row.id, &row.facilityID, &row.isExclusive, &row.start, &row.end, &row.createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, errNoRowsCancelled
	}
	if err != nil {
		return row, store.Classify(err)
	}
	return row, nil
}

// loadClosure reads the closure being acted on. A booking id, or an id that does
// not exist at all, is a 404 here: this endpoint operates on closures, and
// letting it reach a student's booking would make it a second, unauthorised
// cancel path.
func loadClosure(ctx context.Context, q store.Querier, id uuid.UUID) (*Closure, error) {
	var (
		c      Closure
		reason *string
	)
	err := q.QueryRow(ctx, queries.Get(queries.ClosureGet), id).Scan(
		&c.ID, &c.FacilityID, &c.FacilityName, &c.IsExclusive,
		&c.Start, &c.End, &c.Status, &c.CreatedAt, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: closure %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("booking: closure load: %w", err)
	}
	if reason != nil {
		c.Reason = *reason
	}
	return &c, nil
}
