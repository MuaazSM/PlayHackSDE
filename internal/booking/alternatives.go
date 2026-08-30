package booking

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Making losing useful. IMPLEMENTATION.md §5.3 and §10.3, FR-07, G-3.
//
// At 6 PM most people lose, so the 409 is the majority experience and a bare one
// is a dead end: the student is told no and handed nothing to do about it. A 409
// carrying two or three bookable alternatives converts the loss into a second
// attempt, and the second attempt usually succeeds because the herd is fighting
// over ONE slot, not over the evening.
//
// Three rules govern everything in this file, and they are all about not
// damaging the thing that already works:
//
//  1. IT RUNS AFTER ROLLBACK. Once the insert raised 23P01 the transaction is
//     aborted and no further statement runs on that connection (§4.5). Every
//     query here goes to the replica on a fresh connection.
//
//  2. IT IS NOT A READ-THEN-WRITE. Non-negotiable #2 forbids reading occupancy
//     BEFORE a write. This reads occupancy after a write has already failed, to
//     answer "what else?", and its answer is advisory — if a suggested slot is
//     gone by the time the user taps it, the exclusion constraint rejects that
//     attempt too, exactly as it should.
//
//  3. IT IS ON A HARD BUDGET. M-3 puts rejections at p99 < 150 ms, tighter than
//     confirmations on purpose. Enriching an error must never be the reason it is
//     late, so the whole lookup gets AlternativesBudget and degrades to a bare
//     409 on timeout or error. Nice-to-have loses to on-time, every time.
// ---------------------------------------------------------------------------

// MaxAlternatives is how many suggestions a 409 carries. Three is a choice about
// the screen, not the query: a rejected student scanning a list is not shopping,
// and a fourth option costs more attention than it returns.
const MaxAlternatives = 3

// maxSameFacilityAlternatives reserves room for at least one different court.
//
// Without this cap the first question would routinely fill the whole list — a
// court that was free at 18:00 a moment ago usually has 19:00, 20:00 and 21:00
// free too — and the answer "same time, different court" would never be seen,
// even though for someone who lost a 6 PM slot it is often the better one. Two
// and one keeps both kinds of escape on screen.
const maxSameFacilityAlternatives = 2

// AlternativesBudget is the hard ceiling on the whole enrichment, per §5.3.
//
// 40 ms of a 150 ms target. It is a context deadline rather than a hope: the
// queries are cancelled when it expires and the caller ships the bare 409.
const AlternativesBudget = 40 * time.Millisecond

// Alternative is one bookable escape from a lost slot.
type Alternative struct {
	FacilityID uuid.UUID
	Name       string
	Sport      string
	Start      time.Time
	End        time.Time

	// Kind records which question produced this suggestion. The API does not
	// expose it — §10.3 fixes the wire shape at three fields — but it makes the
	// ordering testable and a log line legible.
	Kind string
}

// The two questions, in the order §5.3 asks them.
const (
	// AlternativeLaterHere is the same facility, later today.
	AlternativeLaterHere = "later_same_facility"
	// AlternativeElsewhere is the same sport at the same time, another facility.
	AlternativeElsewhere = "same_time_other_facility"
)

// AlternativesRequest is the slot that was just lost.
type AlternativesRequest struct {
	FacilityID uuid.UUID
	Sport      string
	Start      time.Time
	Duration   time.Duration
}

// End is the exclusive end of the lost window.
func (r AlternativesRequest) End() time.Time { return r.Start.Add(r.Duration) }

// GridCache is the warm campus grid, if there is one.
//
// Narrow on purpose: this path may ONLY read an already-warm cache. A method
// that falls through to Postgres on a miss would silently make the fast path a
// second database round trip inside a 40 ms budget, which is the opposite of
// what it is for. *facility.Availability.CampusCached satisfies this.
type GridCache interface {
	CampusCached(ctx context.Context, date string) (*facility.CampusGrid, bool)
}

// Alternatives answers "what else?" for a request that just lost a slot.
type Alternatives struct {
	replica *pgxpool.Pool
	grid    GridCache
	tz      string
	loc     *time.Location
}

// NewAlternatives builds the lookup.
//
// replica, not primary: this is a read, it is the highest-frequency read there
// is during a burst (one per loser), and the primary's connections belong to the
// write path. tz is the display timezone name — a name rather than a
// *time.Location because Postgres does the day arithmetic and needs the zone by
// name, and because a fixed-offset zone would silently mis-handle a DST boundary.
func NewAlternatives(replica *pgxpool.Pool, grid GridCache, tz string) *Alternatives {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc, tz = time.UTC, "UTC"
	}
	return &Alternatives{replica: replica, grid: grid, tz: tz, loc: loc}
}

// For returns up to MaxAlternatives suggestions, most useful first.
//
// FAST PATH FIRST. If the campus grid of §5.2 is warm in Redis, both questions
// are answered by an in-memory scan of a payload that is already in this
// process — no Postgres, no network, ~0 ms. During a 6 PM burst that cache is
// warm by definition: every student loading the discovery screen refreshed it
// seconds ago. The SQL below is the fallback for a cold cache, not the default.
//
// The caller supplies the deadline. For does not impose one, because the budget
// belongs to the response it is decorating, not to this lookup.
func (a *Alternatives) For(ctx context.Context, req AlternativesRequest) ([]Alternative, error) {
	if req.Duration <= 0 {
		return nil, nil
	}

	// The local day, because "later today" is a local question and the grid is
	// cached per local date.
	date := req.Start.In(a.loc).Format("2006-01-02")

	if a.grid != nil {
		if grid, ok := a.grid.CampusCached(ctx, date); ok {
			return a.fromGrid(grid, req), nil
		}
	}

	if a.replica == nil {
		return nil, nil
	}
	return a.fromSQL(ctx, req, date)
}

// ---------------------------------------------------------------------------
// Fast path — an in-memory scan of the cached campus grid
// ---------------------------------------------------------------------------

// fromGrid answers both questions from the dense grid, without a query.
//
// The grid is a rectangle of states indexed [facility][slot]. Inactive
// facilities are not rows in it at all and a closure already reads as "closed",
// so both exclusions come free — there is nothing to filter that the read path
// did not already filter.
func (a *Alternatives) fromGrid(g *facility.CampusGrid, req AlternativesRequest) []Alternative {
	var here, elsewhere []Alternative

	for fi, f := range g.Facilities {
		if fi >= len(g.Grid) {
			break // ragged payload; treat the rest as unknown rather than guessing
		}
		row := g.Grid[fi]

		switch {
		case f.ID == req.FacilityID:
			// Question 1: this court, later today.
			for si, slot := range g.Slots {
				if !slot.Start.After(req.Start) {
					continue
				}
				if !runFree(row, g.Slots, si, req.Duration) {
					continue
				}
				here = append(here, Alternative{
					FacilityID: f.ID, Name: f.Name, Sport: f.Sport,
					Start: slot.Start, End: slot.Start.Add(req.Duration),
					Kind: AlternativeLaterHere,
				})
			}

		case f.Sport == req.Sport:
			// Question 2: same sport, same time, a different court.
			si := slotIndex(g.Slots, req.Start)
			if si < 0 || !runFree(row, g.Slots, si, req.Duration) {
				continue
			}
			elsewhere = append(elsewhere, Alternative{
				FacilityID: f.ID, Name: f.Name, Sport: f.Sport,
				Start: req.Start, End: req.End(),
				Kind: AlternativeElsewhere,
			})
		}
	}

	// Nearest first, then a stable name order, so the same loss suggests the
	// same things twice running.
	sort.SliceStable(here, func(i, j int) bool { return here[i].Start.Before(here[j].Start) })
	sort.SliceStable(elsewhere, func(i, j int) bool { return elsewhere[i].Name < elsewhere[j].Name })

	return merge(here, elsewhere)
}

// bookable reports whether a grid cell can still take this booking.
//
// "filling" is bookable — it means a shared facility is down to its last few
// places, not that it is gone. Treating it as unavailable would hide the gym
// from exactly the students most likely to still want it.
func bookable(state string) bool {
	return state == facility.StateFree || state == facility.StateFilling
}

// runFree reports whether the requested duration fits, starting at slot si.
//
// A booking may be longer than one grid cell — the axis steps by the smallest
// granularity on campus — so this walks forward until the run covers the whole
// window. It refuses to bridge a gap in the axis: two cells that are not
// adjacent do not make a bookable run, however free they both are.
func runFree(row []string, slots []facility.GridSlot, si int, d time.Duration) bool {
	if si < 0 || si >= len(slots) || si >= len(row) {
		return false
	}
	need := slots[si].Start.Add(d)

	for i := si; i < len(slots) && i < len(row); i++ {
		if i > si && !slots[i].Start.Equal(slots[i-1].End) {
			return false
		}
		if !bookable(row[i]) {
			return false
		}
		if !slots[i].End.Before(need) {
			return true // the run reaches the end of the requested window
		}
	}
	return false // ran out of day before the window was covered
}

// slotIndex finds the column whose start is exactly t, or -1.
func slotIndex(slots []facility.GridSlot, t time.Time) int {
	for i, s := range slots {
		if s.Start.Equal(t) {
			return i
		}
	}
	return -1
}

// merge assembles the final list: the same court first, capped so that a
// different court always gets a place, then whatever is left over.
func merge(here, elsewhere []Alternative) []Alternative {
	out := make([]Alternative, 0, MaxAlternatives)

	head := here
	if len(head) > maxSameFacilityAlternatives {
		head = head[:maxSameFacilityAlternatives]
	}
	out = append(out, head...)

	for _, alt := range elsewhere {
		if len(out) >= MaxAlternatives {
			break
		}
		out = append(out, alt)
	}

	// Nothing to fill the reserved place with — give it back to question 1
	// rather than returning a short list.
	for _, alt := range here[len(head):] {
		if len(out) >= MaxAlternatives {
			break
		}
		out = append(out, alt)
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// Fallback path — two narrow queries on the replica
// ---------------------------------------------------------------------------

// fromSQL runs the two questions as queries, LIMIT MaxAlternatives total.
//
// Both always run. merge always reserves a place for question 2, so skipping it
// on a full first answer would change what a cold cache returns relative to a
// warm one — and a suggestion list that depends on cache state is a bug report
// waiting to be filed.
//
// Each query is capped at MaxAlternatives rather than at its own share, so
// whichever one has answers can fill the list when the other has none.
func (a *Alternatives) fromSQL(ctx context.Context, req AlternativesRequest, date string) ([]Alternative, error) {
	here, err := a.sameFacility(ctx, req, date, MaxAlternatives)
	if err != nil {
		return nil, err
	}

	elsewhere, err := a.sameSport(ctx, req, date, MaxAlternatives)
	if err != nil {
		return nil, err
	}

	return merge(here, elsewhere), nil
}

func (a *Alternatives) sameFacility(ctx context.Context, req AlternativesRequest, date string, limit int) ([]Alternative, error) {
	rows, err := a.replica.Query(ctx, queries.Get(queries.AlternativesSameFacil),
		req.FacilityID, date, a.tz, interval(req.Duration), req.Start.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("booking: alternatives: same facility: %w", err)
	}
	return scanAlternatives(rows, AlternativeLaterHere)
}

// interval renders a Go duration as a Postgres interval parameter.
//
// pgx has no default codec from time.Duration to interval, so passing one
// straight through fails at encode time rather than at compile time. Explicit
// beats discovering that on the loser path during a demo.
func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

func (a *Alternatives) sameSport(ctx context.Context, req AlternativesRequest, date string, limit int) ([]Alternative, error) {
	rows, err := a.replica.Query(ctx, queries.Get(queries.AlternativesSameSport),
		req.Sport, req.FacilityID, req.Start.UTC(), req.End().UTC(), date, a.tz, limit)
	if err != nil {
		return nil, fmt.Errorf("booking: alternatives: same sport: %w", err)
	}
	return scanAlternatives(rows, AlternativeElsewhere)
}

// rowScanner is the slice of pgx.Rows this file needs.
type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

func scanAlternatives(rows rowScanner, kind string) ([]Alternative, error) {
	defer rows.Close()

	var out []Alternative
	for rows.Next() {
		alt := Alternative{Kind: kind}
		if err := rows.Scan(&alt.FacilityID, &alt.Name, &alt.Sport, &alt.Start, &alt.End); err != nil {
			return nil, fmt.Errorf("booking: alternatives: scan: %w", err)
		}
		out = append(out, alt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("booking: alternatives: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The enriched 409
// ---------------------------------------------------------------------------

// Conflict is a lost slot plus what to do about it.
//
// It wraps the reason rather than replacing it, so every existing
// errors.Is(err, ErrSlotTaken) call site — including httpx's status mapping and
// the race suite's assertions — keeps working unchanged. The enrichment is
// additive by construction; a caller that does not know about alternatives
// cannot be broken by them.
type Conflict struct {
	// Reason is ErrSlotTaken or ErrCapacityFull.
	Reason error

	// FacilityName is the venue they lost, so the message can name it.
	FacilityName string

	// Alternatives is empty when the campus is full, when the lookup timed out,
	// or when no lookup was wired. All three are the same thing to a client: a
	// 409 it must handle on its own.
	Alternatives []Alternative

	// WaitlistAvailable says the user may queue for this exact slot instead.
	WaitlistAvailable bool
}

func (c *Conflict) Error() string {
	if c.FacilityName == "" {
		return c.Reason.Error()
	}
	return fmt.Sprintf("%s: %s", c.FacilityName, c.Reason)
}

// Unwrap exposes the reason, so errors.Is(conflict, ErrSlotTaken) is true.
func (c *Conflict) Unwrap() error { return c.Reason }

// WithAlternatives attaches the 409 enrichment to the write path.
//
// Optional wiring, deliberately. A Service without it still returns a correct,
// fast, bare 409 — which is what the concurrency suite measures, and what the
// system degrades to when the replica is unreachable.
func (s *Service) WithAlternatives(a *Alternatives) *Service {
	s.alts = a
	return s
}

// onConflict turns a lost race into a Conflict carrying somewhere else to go.
//
// Errors that are not conflicts pass through untouched: a validation failure or
// an internal fault has no alternative to offer, and inventing one would be
// noise on a path that is already telling the truth.
func (s *Service) onConflict(ctx context.Context, f *facility.Facility, req CreateRequest, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrSlotTaken) && !errors.Is(err, ErrCapacityFull) {
		return err
	}

	c := &Conflict{
		Reason:       err,
		FacilityName: f.Name,
		// The flag ships now; Phase 8 makes it actionable. An active facility
		// can always be queued for — the queue is a request to be told when the
		// slot frees, and nothing about it can fail the way a booking can.
		WaitlistAvailable: f.IsActive,
	}

	if s.alts == nil {
		return c
	}

	// THE BUDGET, §5.3. Derived from the request context, so a request that is
	// already nearly out of time gets whatever is left rather than 40 ms more.
	ctx, cancel := context.WithTimeout(ctx, AlternativesBudget)
	defer cancel()

	alts, aerr := s.alts.For(ctx, AlternativesRequest{
		FacilityID: f.ID,
		Sport:      f.Sport,
		Start:      req.Start.UTC(),
		Duration:   req.Duration,
	})
	if aerr != nil {
		// Degrade to a bare 409. A timeout here is an expected outcome under
		// load, not a fault: the rejection is the response, the alternatives are
		// decoration, and missing M-3 to decorate an error would be the wrong
		// trade in the one place this system is judged.
		alts = nil
	}
	c.Alternatives = alts

	return c
}
