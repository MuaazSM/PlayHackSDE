package checkin_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/checkin"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestNoShowSweep_MarksNoShow is the base case: a court nobody turned up for is
// released.
//
// THE RELEASE IS THE STATUS CHANGE, and nothing else. NO_SHOW is outside
// no_double_book's predicate (CONFIRMED, HELD, BLOCKED), so the moment the sweep
// commits the slot is bookable again — there is no availability flag to clear,
// because there is no availability flag (non-negotiable #4). The last assertion
// here is the one that proves it: somebody else books the released window.
func TestNoShowSweep_MarksNoShow(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg).noPromotion()

	users := pg.Users(t, 2)
	absentee, other := users[0], users[1]
	start, _ := ciSlotPast(15, time.Hour)
	b := r.bookPast(t, r.court, absentee, start, time.Hour)

	res, err := r.sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.NoShows)
	require.Equal(t, 0, res.Completed)

	require.Equal(t, "NO_SHOW", ciBookingStatus(t, pg, b.ID))
	require.Equal(t, 1, ciEventCount(t, pg, b.ID, "NO_SHOW"),
		"the release must be recorded in the audit trail exactly once")

	// The side effect went through the outbox, inside the sweeping transaction —
	// non-negotiable #7. Nothing was sent from the sweeper itself.
	require.Equal(t, 1, ciOutboxCount(t, pg, outbox.TopicBookingNoShow))

	// A second pass finds nothing. The `status = 'CONFIRMED'` guard makes the
	// sweep self-limiting, so a worker ticking every minute all evening does the
	// work once — asserted BEFORE the re-booking below, which would otherwise
	// give the next pass legitimate work of its own.
	res, err = r.sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, res.NoShows)

	// The court is genuinely free again. bookPast fails the test if the write
	// path refuses, which is exactly what a slot that had not really been
	// released would do — the exclusion constraint would raise 23P01.
	replacement := r.bookPast(t, r.court, other, start, time.Hour)
	require.Equal(t, "CONFIRMED", ciBookingStatus(t, pg, replacement.ID),
		"a released no-show must leave the window bookable")
}

// TestNoShowSweep_SkipsCheckedIn is the whole reason check-in exists. A student
// who scanned the code keeps their court; only the ones who did not are released.
//
// The distinction is drawn by NOT EXISTS against check_ins, not by a flag on the
// booking, so there is no second field that could drift out of agreement with
// the attendance record.
//
// The attended booking lands on COMPLETED rather than staying CONFIRMED, which
// is the other half of §7: its window has closed and it was used.
func TestNoShowSweep_SkipsCheckedIn(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg).lateCheckIn().noPromotion()

	users := pg.Users(t, 2)
	arrived, absent := users[0], users[1]

	// Two adjacent hours on one court: both past, both unattended so far.
	start, _ := ciSlotPast(15, time.Hour)
	attended := r.bookPast(t, r.court, arrived, start, time.Hour)
	missed := r.bookPast(t, r.court, absent, start.Add(time.Hour), time.Hour)

	_, err := r.svc.Redeem(ctx, attended.ID, arrived, r.token(r.court))
	require.NoError(t, err)

	res, err := r.sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.NoShows, "only the unattended booking is released")
	require.Equal(t, 1, res.Completed, "the attended one is retired at its slot end")

	require.Equal(t, "COMPLETED", ciBookingStatus(t, pg, attended.ID),
		"a student who turned up must never be recorded as a no-show")
	require.Equal(t, "NO_SHOW", ciBookingStatus(t, pg, missed.ID))

	require.Equal(t, 0, ciEventCount(t, pg, attended.ID, "NO_SHOW"))
	require.Equal(t, 1, ciEventCount(t, pg, attended.ID, "COMPLETED"))
	require.Equal(t, 1, ciOutboxCount(t, pg, outbox.TopicBookingNoShow),
		"a completed booking releases nothing and announces nothing")
}

// TestNoShowSweep_ReleasesSharedCapacity covers the one facility where the
// status change is not enough.
//
// The gymnasium's occupancy is the slot_capacity counter, not the exclusion
// constraint — shared rows carry is_exclusive = false and are not in that index
// at all — so a no-show there must decrement the counter explicitly. In the SAME
// transaction as the status change, so a sweep that rolled back could not leave
// a place returned for a booking that still stands.
func TestNoShowSweep_ReleasesSharedCapacity(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg).noPromotion()

	users := pg.Users(t, 2)
	start, _ := ciSlotPast(15, time.Hour)

	r.bookPast(t, r.gym, users[0], start, time.Hour)
	present := r.bookPast(t, r.gym, users[1], start, time.Hour)
	require.Equal(t, 2, ciBooked(t, pg, r.gym, start))

	// One of the two turned up.
	r.lateCheckIn()
	_, err := r.svc.Redeem(ctx, present.ID, users[1], r.token(r.gym))
	require.NoError(t, err)

	res, err := r.sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.NoShows)

	require.Equal(t, 1, ciBooked(t, pg, r.gym, start),
		"the absent student's place must be given back to the counter")

	// The counter and the rows still agree, which is the property a drifting
	// release would break.
	var confirmed int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*)::int FROM bookings
		  WHERE facility_id = $1 AND status IN ('CONFIRMED','COMPLETED')`, r.gym).Scan(&confirmed))
	require.Equal(t, 1, confirmed)
}

// TestNoShowSweep_PromotesNextWaitlister is the end-to-end case this phase is
// for: no-show → release → promotion.
//
// The promotion runs through waitlist.Service.Promote — THE SAME method a live
// cancellation and the offer-expiry sweeper call, claiming through the same
// waitlist_claim_head statement. That is why check-in was chosen as the second
// innovation: the machinery already existed, and reusing it is not merely tidy.
// A second "who is next" query here would be an independent reader of the
// WAITING rows, and two independent readers eventually hand one student two
// courts.
//
// The offer is a real HELD booking, so it genuinely reserves the released window
// rather than promising it — HELD is inside no_double_book's predicate, and the
// last assertion proves an outsider cannot take it.
func TestNoShowSweep_PromotesNextWaitlister(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg) // promotion attached

	users := pg.Users(t, 3)
	absentee, waiter, outsider := users[0], users[1], users[2]
	start, end := ciSlotPast(15, time.Hour)

	b := r.bookPast(t, r.court, absentee, start, time.Hour)

	entry, err := r.queue.Join(ctx, waiter, r.court, start, end)
	require.NoError(t, err)
	require.Equal(t, 1, entry.Place)

	res, err := r.sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.NoShows)
	require.Equal(t, 1, res.PromotionsAttempted)

	require.Equal(t, "NO_SHOW", ciBookingStatus(t, pg, b.ID))
	require.Equal(t, "PROMOTED", ciWaitlistStatus(t, pg, entry.ID),
		"the released window must go to the head of the queue in the same transaction")

	// The offer is backed by a hold on the SAME window, pointing at the queue
	// entry that produced it.
	var (
		heldID    uuid.UUID
		heldUser  uuid.UUID
		heldStart time.Time
	)
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT b.id, b.user_id, lower(b.during)
		   FROM bookings b JOIN waitlist w ON w.booking_id = b.id
		  WHERE w.id = $1 AND b.status = 'HELD'`, entry.ID).
		Scan(&heldID, &heldUser, &heldStart))
	require.Equal(t, waiter, heldUser)
	require.True(t, heldStart.Equal(start))

	// Announced in the right order: the release before the offer. Outbox rows in
	// one transaction share a created_at, so the id is the tiebreaker — which is
	// what makes a watching grid see free-then-held rather than a slot that
	// appears to stay free.
	var noShowID, promotedID int64
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT id FROM outbox WHERE topic = $1`, outbox.TopicBookingNoShow).Scan(&noShowID))
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT id FROM outbox WHERE topic = $1`, outbox.TopicWaitlistPromoted).Scan(&promotedID))
	require.Less(t, noShowID, promotedID)

	// And the hold is a real reservation: the released window is not free for
	// anybody else. The exclusion constraint says so — HELD is inside
	// no_double_book's predicate precisely so a promotion offer holds the court
	// it was offered for.
	_, err = r.tryBookPast(r.court, outsider, start, time.Hour)
	require.ErrorIs(t, err, booking.ErrSlotTaken,
		"a promoted hold must actually reserve the released court")
}

// TestNoShowSweep_ConcurrentRunsIdempotent runs two sweepers at the same instant
// against the same overdue bookings.
//
// This is the deployment reality, not a contrived case: EMBED_WORKERS=true means
// every api replica runs its own sweeper, so N replicas is N sweepers ticking on
// the same minute.
//
// Each booking must be handled EXACTLY once — one status change, one audit row,
// one outbox row. FOR UPDATE SKIP LOCKED inside booking_mark_no_show is what
// gives the two passes disjoint sets of rows instead of one waiting on the other;
// the `status = 'CONFIRMED'` predicate is what makes double-handling impossible
// even if they did overlap.
//
// Run several rounds: a locking bug here is probabilistic, and one green run
// says only that the two passes happened not to interleave.
func TestNoShowSweep_ConcurrentRunsIdempotent(t *testing.T) {
	const (
		rounds   = 5
		bookings = 6
	)

	pg := testutil.Postgres(t)

	for round := 1; round <= rounds; round++ {
		pg.Reset(t)
		r := newCIRig(t, pg).noPromotion()
		pg.Warm(t, 8)

		users := pg.Users(t, bookings)
		base, _ := ciSlotPast(15, time.Hour)

		ids := make([]uuid.UUID, bookings)
		for i := 0; i < bookings; i++ {
			b := r.bookPast(t, r.court, users[i], base.Add(time.Duration(i)*time.Hour), time.Hour)
			ids[i] = b.ID
		}

		out := testutil.Race(t, 2, func(ctx context.Context, _ int) (any, error) {
			return r.sweeper.Sweep(ctx)
		})
		require.Empty(t, out.Failures(), "round %d: both sweeps must succeed: %v", round, out.Failures())

		// Between them the two passes released every booking, and released each
		// one only once — so the totals sum to the number of bookings rather than
		// to twice it.
		total := 0
		for _, a := range out.Attempts {
			res, ok := a.Value.(checkin.SweepResult)
			require.True(t, ok, "round %d: unexpected sweep result %T", round, a.Value)
			total += res.NoShows
		}
		require.Equal(t, bookings, total,
			"round %d: the two sweeps together must release each booking once", round)

		for _, id := range ids {
			require.Equal(t, "NO_SHOW", ciBookingStatus(t, pg, id), "round %d", round)
			require.Equal(t, 1, ciEventCount(t, pg, id, "NO_SHOW"),
				"round %d: booking %s was released twice", round, id)
		}
		require.Equal(t, bookings, ciOutboxCount(t, pg, outbox.TopicBookingNoShow),
			"round %d: one notification per released court, no more", round)
	}
}
