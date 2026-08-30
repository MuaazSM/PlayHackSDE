// Package policy is the fair-use layer: how many courts one student may hold at
// once, how many hours a week they may take, and where they sit in a queue.
//
// It is deliberately the SOFTEST thing in this codebase, and the contrast is the
// point. Slot uniqueness is enforced by a Postgres exclusion constraint and is
// absolute: no amount of concurrency can produce two confirmed bookings for one
// court and hour. A fair-use cap is a policy knob whose worst possible failure
// is one student holding four bookings instead of three. Those two facts do not
// deserve the same machinery, and spending an exclusion constraint on the second
// would blur the answer to the only question that matters here — where does
// correctness live? (IMPLEMENTATION.md §4.7.)
//
// So: Check reads, then the caller writes, inside one transaction, with nothing
// serialising the pair. See Check's doc comment.
package policy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// WeeklyWindow is the width of the rolling window max_weekly_hours is measured
// over.
//
// ROLLING, not calendar. A calendar week resets at a boundary everybody can see,
// so the cap becomes "ten hours, plus ten more at midnight on Sunday" and the
// student who books across the boundary beats the student who does not. Seven
// days back from wherever now() happens to be has no boundary to game.
const WeeklyWindow = 7 * 24 * time.Hour

// NoShowLookback is how long a no-show keeps costing a student queue priority.
//
// Long enough that "repeat" means something and short enough to be forgiven
// within a semester. This is the feedback loop §7 feeds into §11: not turning up
// to a court someone else wanted moves you down the queue for the next one.
//
// Deliberately NOT policies.no_show_penalty_days. That column is a suspension
// length — how long a repeat offender is barred from booking at all — which
// nothing implements yet, and it is seeded to 0. Deriving the priority penalty
// from it would leave this feature switched off in every seeded database and
// silently on in some others.
const NoShowLookback = 30 * 24 * time.Hour

// MaxNoShowPenalty bounds how far no-shows can push somebody down.
//
// Bounded on purpose. Unbounded, one bad month would sink a student below every
// possible queue forever, and a fairness mechanism that cannot be recovered from
// is a punishment mechanism. Two is exactly the span of the tiers, so the worst
// case is that an institute team queues like an individual — never worse than
// the bottom of the honest ranking.
const MaxNoShowPenalty = 2

// Limit names, as they appear in the 422 body. The client switches on these.
const (
	LimitForwardBookings = "max_forward_bookings"
	LimitWeeklyHours     = "max_weekly_hours"
)

// ErrPolicyExceeded means a fair-use cap was hit. Advisory — see §4.7.
//
// Callers match this with errors.Is; the concrete *LimitError reachable with
// errors.As says WHICH cap and when it frees up.
var ErrPolicyExceeded = errors.New("policy limit exceeded")

// LimitError is a refused booking, named.
//
// A bare "you have hit a limit" is useless to a student: they cannot tell
// whether to cancel something, wait, or pick a different sport. Limit says which
// rule stopped them and ResetsAt says when it stops applying, which between them
// answer "what do I do now".
type LimitError struct {
	// Limit is the policy field that was hit — one of the Limit* constants.
	Limit string

	// ResetsAt is when this limit next admits another booking: the moment the
	// earliest booking counted against it leaves the window.
	ResetsAt time.Time

	// Allowed and Used are the cap and the current consumption, in the limit's
	// own units (bookings, or hours).
	Allowed float64
	Used    float64
}

func (e *LimitError) Error() string {
	switch e.Limit {
	case LimitForwardBookings:
		return fmt.Sprintf("you already hold %d upcoming bookings; the limit is %d",
			int(e.Used), int(e.Allowed))
	case LimitWeeklyHours:
		return fmt.Sprintf("that would put you at %.1f booked hours in seven days; the limit is %.0f",
			e.Used, e.Allowed)
	default:
		return "fair-use limit reached"
	}
}

// Unwrap makes errors.Is(err, ErrPolicyExceeded) true for every cap.
func (e *LimitError) Unwrap() error { return ErrPolicyExceeded }

// Window is the [start, end) a booking would occupy — the `during` column, in
// Go. Half-open, like everything else that touches a tstzrange here: 18:00-19:00
// and 19:00-20:00 do not overlap.
type Window struct {
	Start time.Time
	End   time.Time
}

// Duration is how long the window is.
func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }

// Limits is one resolved policy row.
type Limits struct {
	// FacilitySpecific is true when this came from a facility override rather
	// than the global row.
	FacilitySpecific bool

	MaxForwardBookings int
	MaxWeeklyHours     int
	NoShowPenaltyDays  int
}

// enforces reports whether this row constrains anything at all.
//
// A non-positive cap means UNLIMITED for that field, not "zero allowed". Two
// reasons. It keeps "no policy row" and "no value" meaning the same thing, so
// there is one way to express "do not enforce this". And it makes the failure
// mode of a half-filled configuration row a permissive one — a fair-use knob
// that silently bricked every booking on a facility because somebody left a
// column at 0 would be a far worse outage than the one it prevented.
func (l Limits) enforces() bool { return l.MaxForwardBookings > 0 || l.MaxWeeklyHours > 0 }

// CheckFunc is Check's signature, so a caller can hold the check as a value
// without depending on how it is built. internal/booking holds one of these.
type CheckFunc func(ctx context.Context, tx pgx.Tx, userID, facilityID uuid.UUID, during Window) error

// Check asserts the caller's fair-use caps, inside the caller's transaction.
//
// ADVISORY, DELIBERATELY: this is a read-then-write with no lock between the
// two halves, so two perfectly simultaneous requests can both observe "2 of 3
// used" and both commit, leaving one student with 4 bookings against a cap of 3
// — and that is accepted (IMPLEMENTATION.md §4.7), because a fair-use quota is a
// policy knob whose worst failure is one extra booking for one user, while the
// slot invariant it sits next to is enforced by the database engine and is
// absolute. Do not "fix" this with an exclusion constraint, an advisory lock or
// a SELECT FOR UPDATE: it would buy a soft quota a hard guarantee at the cost of
// the story about where correctness actually lives.
//
// It takes a pgx.Tx rather than a pool or a store.Querier so that "inside the
// transaction" is a compile-time property. A cap evaluated on a separate
// connection would be evaluating a different database state from the insert it
// is supposed to gate, and would keep counting a booking that then rolled back.
//
// Ordinary database failures are returned classified and are NOT policy
// violations; only *LimitError satisfies errors.Is(err, ErrPolicyExceeded).
func Check(ctx context.Context, tx pgx.Tx, userID, facilityID uuid.UUID, during Window) error {
	limits, err := Resolve(ctx, tx, facilityID)
	if err != nil {
		return err
	}
	// No policy row anywhere, or one that constrains nothing: unlimited. Costs
	// one indexed lookup on a table with a handful of rows and buys the ability
	// to run the whole system with fair use switched off.
	if limits == nil || !limits.enforces() {
		return nil
	}

	// A facility override governs THAT facility, so it counts only that
	// facility's bookings. The global row sees the whole campus.
	var scope *uuid.UUID
	if limits.FacilitySpecific {
		scope = &facilityID
	}

	usage, err := usageFor(ctx, tx, userID, scope)
	if err != nil {
		return err
	}

	if limits.MaxForwardBookings > 0 && usage.ForwardCount >= limits.MaxForwardBookings {
		return &LimitError{
			Limit: LimitForwardBookings,
			// When the earliest one starts, it stops being forward.
			ResetsAt: usage.resetOr(usage.ForwardResetsAt, usage.AsOf.Add(WeeklyWindow)),
			Allowed:  float64(limits.MaxForwardBookings),
			Used:     float64(usage.ForwardCount),
		}
	}

	if limits.MaxWeeklyHours > 0 {
		// The booking being requested only counts if it lands inside the window.
		// Something booked five weeks out is not this week's problem, and the
		// query would not count it on the next request either — charging for it
		// here and not there would make the cap depend on the order of requests.
		requested := time.Duration(0)
		if during.Start.Before(usage.AsOf.Add(WeeklyWindow)) {
			requested = during.Duration()
		}

		total := usage.WindowBooked + requested
		allowed := time.Duration(limits.MaxWeeklyHours) * time.Hour
		if total > allowed {
			return &LimitError{
				Limit: LimitWeeklyHours,
				// When the earliest counted booking ends, its hours come back.
				// With nothing counted yet the request is single-handedly over
				// the cap and no amount of waiting helps; the end of the window
				// is the honest "not before then".
				ResetsAt: usage.resetOr(usage.WindowResetsAt, usage.AsOf.Add(WeeklyWindow)),
				Allowed:  float64(limits.MaxWeeklyHours),
				Used:     total.Hours(),
			}
		}
	}

	return nil
}

// Resolve returns the policy governing a facility: its own row if it has one,
// otherwise the global row, otherwise nil for "no policy configured".
//
// nil is not an error. See policy_resolve.sql for why the absent case is
// permissive rather than a refusal.
func Resolve(ctx context.Context, q store.Querier, facilityID uuid.UUID) (*Limits, error) {
	var l Limits
	err := q.QueryRow(ctx, queries.Get(queries.PolicyResolve), facilityID).Scan(
		&l.FacilitySpecific, &l.MaxForwardBookings, &l.MaxWeeklyHours, &l.NoShowPenaltyDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("policy: resolve: %w", store.Classify(err))
	}
	return &l, nil
}

// usage is one student's current consumption, as of the transaction's now().
type usage struct {
	// AsOf is the transaction's clock. Everything else is relative to it, and
	// comparisons in Go use it rather than time.Now so the check cannot straddle
	// two different clocks.
	AsOf time.Time

	ForwardCount    int
	ForwardResetsAt *time.Time

	// WindowBooked is time already committed inside the rolling window.
	WindowBooked   time.Duration
	WindowResetsAt *time.Time
}

// resetOr dereferences a nullable reset instant, falling back when nothing was
// counted.
func (u usage) resetOr(at *time.Time, fallback time.Time) time.Time {
	if at != nil {
		return at.UTC()
	}
	return fallback.UTC()
}

func usageFor(ctx context.Context, tx pgx.Tx, userID uuid.UUID, scope *uuid.UUID) (*usage, error) {
	var (
		u       usage
		seconds int64
	)
	err := tx.QueryRow(ctx, queries.Get(queries.PolicyUsage),
		userID, scope, int(WeeklyWindow/(24*time.Hour)),
	).Scan(&u.AsOf, &u.ForwardCount, &u.ForwardResetsAt, &seconds, &u.WindowResetsAt)
	if err != nil {
		return nil, fmt.Errorf("policy: usage: %w", store.Classify(err))
	}
	u.AsOf = u.AsOf.UTC()
	u.WindowBooked = time.Duration(seconds) * time.Second
	return &u, nil
}

// ---------------------------------------------------------------------------
// Priority tiers — §11
// ---------------------------------------------------------------------------

// Tier is why one student queues ahead of another.
//
// The tiers are ordered by how many people a missed slot lets down: an institute
// team practice is thirty students and a fixture to prepare for, a hostel team
// is a dozen, an individual is one. That is the whole justification, and it is
// worth being able to say out loud — a queue that reorders itself for reasons
// nobody can state is just favouritism with a database behind it.
type Tier string

const (
	TierIndividual    Tier = "INDIVIDUAL"
	TierHostelTeam    Tier = "HOSTEL_TEAM"
	TierInstituteTeam Tier = "INSTITUTE_TEAM"
)

// Base is the tier's contribution to waitlist.priority.
//
// This ranking exists ONCE, here. Migration 0010 stores which tier a student is
// and says nothing about their order, so there is no second copy of this table
// in SQL to drift out of agreement with it.
func (t Tier) Base() int {
	switch t {
	case TierInstituteTeam:
		return 2
	case TierHostelTeam:
		return 1
	default:
		// Including an unrecognised value: an unknown tier queues as an
		// individual rather than inheriting somebody else's advantage.
		return 0
	}
}

// Priority is the number that goes in waitlist.priority — the tier's base, less
// a bounded penalty for recent no-shows.
//
// waitlist_claim_head orders by (priority DESC, position ASC), so a higher
// number is promoted sooner and FIFO still decides within a tier.
//
// This is where §7 feeds back into fairness: a student who books a court at
// 6 PM and does not turn up has taken it from somebody who would have. Counting
// their no-shows against their queue position is the only consequence in the
// system that is proportionate — it costs them the next contended slot, not the
// ability to book at all.
//
// Read outside any transaction on purpose: it decides where somebody sits in a
// queue, not whether a write is allowed, so a slightly stale count costs a
// student one place and nothing else.
func Priority(ctx context.Context, q store.Querier, userID uuid.UUID) (int, error) {
	var (
		tier     string
		noShows  int
		lookback = int(NoShowLookback / (24 * time.Hour))
	)
	err := q.QueryRow(ctx, queries.Get(queries.PolicyPriority), userID, lookback).
		Scan(&tier, &noShows)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("policy: priority: %w: user %s", store.ErrNotFound, userID)
	}
	if err != nil {
		return 0, fmt.Errorf("policy: priority: %w", store.Classify(err))
	}

	if noShows > MaxNoShowPenalty {
		noShows = MaxNoShowPenalty
	}
	return Tier(tier).Base() - noShows, nil
}
