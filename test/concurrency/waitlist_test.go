package concurrency_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/waitlist"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// wlQuiet keeps the promotion log out of the test output. The sweeper and the
// promotion path both log at info; twenty rounds of the race test would bury
// the failure message that matters.
func wlQuiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// wlSlot returns [start, end) at TOMORROW's given IST hour, in UTC.
//
// Tomorrow rather than today because booking.Create refuses a start in the
// past: a test pinned to today's 18:00 passes in the morning and fails after
// dinner, which is the worst kind of flake — it looks like a concurrency bug.
func wlSlot(hour int, d time.Duration) (start, end time.Time) {
	tomorrow := time.Now().In(testutil.IST).AddDate(0, 0, 1)
	start = time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), hour, 0, 0, 0, testutil.IST).UTC()
	return start, start.Add(d)
}

// wlRig is one test's worth of wiring: a catalogue, a queue and a booking
// service with the promotion hook attached.
type wlRig struct {
	pg    *testutil.PG
	cat   *facility.Repo
	queue *waitlist.Service
	svc   *booking.Service
	court uuid.UUID
}

// newWLRig builds the rig. ttl is the promotion claim window; pass a NEGATIVE
// duration to mint offers that are already expired, which is how the expiry
// paths are tested without sleeping.
func newWLRig(t *testing.T, pg *testutil.PG, ttl time.Duration) *wlRig {
	t.Helper()

	cat := testutil.Catalogue(t, pg)
	court := testutil.CourtID()
	testutil.WarmCatalogue(t, cat, court)

	queue := waitlist.NewService(pg.DB, cat, ttl, wlQuiet())
	svc := pg.BookingServiceWith(t, cat).WithPromotion(queue)

	return &wlRig{pg: pg, cat: cat, queue: queue, svc: svc, court: court}
}

func (r *wlRig) book(t *testing.T, user uuid.UUID, start time.Time, d time.Duration) *booking.Booking {
	t.Helper()
	b, err := r.svc.Create(context.Background(), booking.CreateRequest{
		FacilityID: r.court, UserID: user, Start: start,
		Duration: d, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)
	return b
}

func (r *wlRig) join(t *testing.T, user uuid.UUID, start, end time.Time) *waitlist.Entry {
	t.Helper()
	e, err := r.queue.Join(context.Background(), user, r.court, start, end)
	require.NoError(t, err)
	return e
}

// wlOffer is one row of the join between a queue entry and the hold it points
// at. Every assertion in this file reads the database rather than a return
// value: the guarantee under test is a property of the rows, and a test that
// trusted what the service told it would pass even if nothing had been written.
type wlOffer struct {
	EntryID       uuid.UUID
	UserID        uuid.UUID
	EntryStatus   string
	BookingID     uuid.UUID
	BookingStatus string
	Start         time.Time
	HeldUntilSet  bool
}

// wlOffers returns every queue entry that has been given a booking, newest
// last. Entries still WAITING are excluded — they have no offer.
func wlOffers(t *testing.T, pg *testutil.PG) []wlOffer {
	t.Helper()

	rows, err := pg.Pool.Query(context.Background(), `
		SELECT w.id, w.user_id, w.status::text,
		       b.id, b.status::text, lower(b.during), b.held_until IS NOT NULL
		  FROM waitlist w
		  JOIN bookings b ON b.id = w.booking_id
		 ORDER BY lower(b.during), w.position`)
	require.NoError(t, err)
	defer rows.Close()

	var out []wlOffer
	for rows.Next() {
		var o wlOffer
		require.NoError(t, rows.Scan(&o.EntryID, &o.UserID, &o.EntryStatus,
			&o.BookingID, &o.BookingStatus, &o.Start, &o.HeldUntilSet))
		out = append(out, o)
	}
	require.NoError(t, rows.Err())
	return out
}

// wlEntryStatus reads one queue entry's status.
func wlEntryStatus(t *testing.T, pg *testutil.PG, id uuid.UUID) string {
	t.Helper()
	var s string
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT status::text FROM waitlist WHERE id = $1`, id).Scan(&s))
	return s
}

// wlBookingStatus reads one booking's status.
func wlBookingStatus(t *testing.T, pg *testutil.PG, id uuid.UUID) string {
	t.Helper()
	var s string
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT status::text FROM bookings WHERE id = $1`, id).Scan(&s))
	return s
}

// wlHoldCount counts live holds — the rows that actually reserve a court.
func wlHoldCount(t *testing.T, pg *testutil.PG) int {
	t.Helper()
	var n int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM bookings WHERE status = 'HELD'`).Scan(&n))
	return n
}

// ---------------------------------------------------------------------------
// The one that matters
// ---------------------------------------------------------------------------

// TestConcurrentCancels_DistinctPromotions is the second concurrency proof.
//
// Three students cancel three slots at the same instant. All three cancelling
// transactions run waitlist_claim_head against the SAME queue — every waiting
// entry spans all three freed hours, so there is genuinely one queue and not
// three — and three DIFFERENT students must come off it, each holding the
// window whose cancel promoted them.
//
// Run twenty times, deliberately. A lock bug here is probabilistic: it needs
// the three transactions to interleave in a particular way, and a single green
// run says only that they happened not to.
//
// What this test does and does not pin down, stated honestly rather than
// implied: it proves the OUTCOME — three cancels, three distinct promotions,
// three distinct windows, every offer backed by a real hold. It does not by
// itself prove that SKIP LOCKED is carrying that, because the row lock and the
// WAITING predicate keep the promotions distinct even with a plain FOR UPDATE
// (verified by mutation: this test still passes with SKIP LOCKED removed).
// TestPromotion_SkipsLockedEntries is the one that fails on that mutation.
// Both are needed; neither replaces the other.
func TestConcurrentCancels_DistinctPromotions(t *testing.T) {
	const rounds = 20

	pg := testutil.Postgres(t)

	for round := 1; round <= rounds; round++ {
		pg.Reset(t)
		r := newWLRig(t, pg, 10*time.Minute)
		pg.Warm(t, 10)

		users := pg.Users(t, 6)
		owners, waiters := users[:3], users[3:]

		// Three ADJACENT hours: three bookings that can coexist on one exclusive
		// court (overlapping ones could not — the constraint would reject them).
		base, _ := wlSlot(15, time.Hour)
		bookings := make([]*booking.Booking, 3)
		for i, owner := range owners {
			bookings[i] = r.book(t, owner, base.Add(time.Duration(i)*time.Hour), time.Hour)
		}

		// Every waiter queues for the WHOLE three-hour window, so each of the
		// three freed hours overlaps each of the three entries. Without that,
		// the three cancels would be reading three separate queues and SKIP
		// LOCKED would never be exercised.
		entries := make([]*waitlist.Entry, 3)
		for i, w := range waiters {
			entries[i] = r.join(t, w, base, base.Add(3*time.Hour))
		}

		out := testutil.Race(t, 3, func(ctx context.Context, i int) (any, error) {
			return r.svc.Cancel(ctx, bookings[i].ID, owners[i], "race")
		})
		require.Empty(t, out.Failures(), "round %d: every cancel must succeed: %v", round, out.Failures())

		offers := wlOffers(t, pg)
		require.Len(t, offers, 3,
			"round %d: three cancels must produce three promotions, got %d", round, len(offers))

		seenUsers := map[uuid.UUID]bool{}
		seenSlots := map[time.Time]bool{}
		for _, o := range offers {
			require.Equal(t, "PROMOTED", o.EntryStatus)
			require.Equal(t, "HELD", o.BookingStatus,
				"round %d: an offer must be backed by a hold that reserves the court", round)
			require.True(t, o.HeldUntilSet, "round %d: a hold must carry its deadline", round)
			require.False(t, seenUsers[o.UserID],
				"round %d: user %s was promoted twice — SKIP LOCKED is not spreading claimants", round, o.UserID)
			require.False(t, seenSlots[o.Start],
				"round %d: two promotions into the same window", round)
			seenUsers[o.UserID] = true
			seenSlots[o.Start] = true
		}

		// Every entry was used exactly once, and none is still waiting.
		for _, e := range entries {
			require.Equal(t, "PROMOTED", wlEntryStatus(t, pg, e.ID), "round %d", round)
		}

		// And the cancellations themselves stand.
		for _, b := range bookings {
			require.Equal(t, "CANCELLED", wlBookingStatus(t, pg, b.ID), "round %d", round)
		}
	}
}

// TestPromotion_SkipsLockedEntries is the mutation test for SKIP LOCKED.
//
// Another transaction holds the head of the queue — which in production is
// simply another cancel, or the sweeper, part-way through its own promotion.
// With SKIP LOCKED the cancel steps over that row, promotes the next student
// and commits. Without it, the cancel BLOCKS on the lock for as long as the
// holder lives, dragging an unrelated student's cancellation along with it;
// here the holder outlives the deadline, so the cancel fails outright and this
// test goes red.
//
// Deterministic by construction: the lock is definitely held before the cancel
// starts, so there is no timing window to lose.
func TestPromotion_SkipsLockedEntries(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, 10*time.Minute)

	users := pg.Users(t, 3)
	owner, head, next := users[0], users[1], users[2]
	start, end := wlSlot(15, time.Hour)

	b := r.book(t, owner, start, time.Hour)
	e1 := r.join(t, head, start, end)
	e2 := r.join(t, next, start, end)

	// Pin the head of the queue in a transaction that outlives the cancel.
	holder, err := pg.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(ctx) }()

	var locked uuid.UUID
	require.NoError(t, holder.QueryRow(ctx,
		`SELECT id FROM waitlist WHERE id = $1 FOR UPDATE`, e1.ID).Scan(&locked))

	// Generous enough that a slow machine does not fail it, short enough that a
	// cancel waiting on the holder cannot pass it.
	cancelCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()

	_, err = r.svc.Cancel(cancelCtx, b.ID, owner, "")
	require.NoError(t, err,
		"a cancel must not wait on a queue entry another transaction is holding")

	require.Equal(t, "WAITING", wlEntryStatus(t, pg, e1.ID),
		"the locked entry must be stepped over, not taken")
	require.Equal(t, "PROMOTED", wlEntryStatus(t, pg, e2.ID),
		"the next claimable student must get the freed window")
	require.Equal(t, 1, wlHoldCount(t, pg))
}

// ---------------------------------------------------------------------------
// Ordering
// ---------------------------------------------------------------------------

// TestPromotion_RespectsPriorityThenPosition pins the queue's order: priority
// tiers first, FIFO within a tier.
//
// The FIFO half is the one worth guarding. position is a bigserial, so the
// order is whatever the sequence handed out — if promotion ever started
// ordering by created_at, or by nothing at all, two students who joined a
// millisecond apart could swap places and nobody would notice until somebody
// complained they had been skipped.
func TestPromotion_RespectsPriorityThenPosition(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, 10*time.Minute)

	users := pg.Users(t, 5)
	owners, waiters := users[:2], users[2:]

	base, _ := wlSlot(15, time.Hour)
	first := r.book(t, owners[0], base, time.Hour)
	second := r.book(t, owners[1], base.Add(time.Hour), time.Hour)

	// Joined in order: positions ascend with the sequence.
	e1 := r.join(t, waiters[0], base, base.Add(2*time.Hour))
	e2 := r.join(t, waiters[1], base, base.Add(2*time.Hour))
	e3 := r.join(t, waiters[2], base, base.Add(2*time.Hour))
	require.Less(t, e1.Position, e2.Position)
	require.Less(t, e2.Position, e3.Position)

	// The last to join is promoted first, because priority outranks position.
	_, err := pg.Pool.Exec(ctx, `UPDATE waitlist SET priority = 5 WHERE id = $1`, e3.ID)
	require.NoError(t, err)

	_, err = r.svc.Cancel(ctx, first.ID, owners[0], "")
	require.NoError(t, err)
	require.Equal(t, "PROMOTED", wlEntryStatus(t, pg, e3.ID), "the priority tier goes first")
	require.Equal(t, "WAITING", wlEntryStatus(t, pg, e1.ID))
	require.Equal(t, "WAITING", wlEntryStatus(t, pg, e2.ID))

	// With the tiers level again, the earliest position wins.
	_, err = r.svc.Cancel(ctx, second.ID, owners[1], "")
	require.NoError(t, err)
	require.Equal(t, "PROMOTED", wlEntryStatus(t, pg, e1.ID), "FIFO within a tier")
	require.Equal(t, "WAITING", wlEntryStatus(t, pg, e2.ID))
}

// ---------------------------------------------------------------------------
// A hold is a real reservation
// ---------------------------------------------------------------------------

// TestPromotion_HeldRowBlocksNewBookings is why an offer is a booking row.
//
// If the promotion only wrote "you may have this slot" onto the queue entry,
// the court would read as free and the next student to open the grid would take
// it out from under the person who was just offered it. HELD is inside
// no_double_book's predicate precisely so that cannot happen.
func TestPromotion_HeldRowBlocksNewBookings(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, 10*time.Minute)

	users := pg.Users(t, 3)
	owner, waiter, outsider := users[0], users[1], users[2]

	start, end := wlSlot(15, time.Hour)
	b := r.book(t, owner, start, time.Hour)
	r.join(t, waiter, start, end)

	_, err := r.svc.Cancel(ctx, b.ID, owner, "")
	require.NoError(t, err)
	require.Equal(t, 1, wlHoldCount(t, pg))

	// The court is cancelled but NOT free: it is being held for the promoted
	// student, and the exclusion constraint says so.
	_, err = r.svc.Create(ctx, booking.CreateRequest{
		FacilityID: r.court, UserID: outsider, Start: start,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.ErrorIs(t, err, booking.ErrSlotTaken,
		"a promotion offer must actually reserve the slot, not merely promise it")
}

// TestPromotion_HeldShowsAsHeldInAvailability checks the student-facing half of
// the same fact: the grid distinguishes "somebody's, for now" from "taken" and
// from "free", and it derives that from the bookings table like everything else.
func TestPromotion_HeldShowsAsHeldInAvailability(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, 10*time.Minute)

	users := pg.Users(t, 2)
	owner, waiter := users[0], users[1]

	start, end := wlSlot(15, time.Hour)
	b := r.book(t, owner, start, time.Hour)
	r.join(t, waiter, start, end)

	f, err := r.cat.Get(ctx, r.court)
	require.NoError(t, err)
	avail := facility.NewAvailability(pg.DB.Replica, nil, "Asia/Kolkata", wlQuiet())
	date := start.In(testutil.IST).Format("2006-01-02")

	day, err := avail.ForFacility(ctx, f, date)
	require.NoError(t, err)
	require.Equal(t, facility.StateBooked, wlStateAt(t, day, start))

	_, err = r.svc.Cancel(ctx, b.ID, owner, "")
	require.NoError(t, err)

	day, err = avail.ForFacility(ctx, f, date)
	require.NoError(t, err)
	require.Equal(t, facility.StateHeld, wlStateAt(t, day, start),
		"a promoted hold must read as held, not as free and not as booked")
}

// wlStateAt finds the grid cell for a slot start.
func wlStateAt(t *testing.T, day *facility.DayAvailability, start time.Time) string {
	t.Helper()
	for _, s := range day.Slots {
		if s.Start.Equal(start) {
			return s.State
		}
	}
	t.Fatalf("no slot at %s in the day grid", start)
	return ""
}

// ---------------------------------------------------------------------------
// Claim — §6.3
// ---------------------------------------------------------------------------

// wlPromoteOne cancels a booking and returns the single offer that produced.
func wlPromoteOne(t *testing.T, r *wlRig, b *booking.Booking, owner uuid.UUID) wlOffer {
	t.Helper()
	_, err := r.svc.Cancel(context.Background(), b.ID, owner, "")
	require.NoError(t, err)

	offers := wlOffers(t, r.pg)
	require.Len(t, offers, 1)
	return offers[0]
}

func TestClaim_ConvertsHeldToConfirmed(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, 10*time.Minute)

	users := pg.Users(t, 2)
	owner, waiter := users[0], users[1]
	start, end := wlSlot(15, time.Hour)

	r.join(t, waiter, start, end)
	offer := wlPromoteOne(t, r, r.book(t, owner, start, time.Hour), owner)
	require.Equal(t, waiter, offer.UserID)

	claimed, err := r.svc.Claim(ctx, offer.BookingID, waiter)
	require.NoError(t, err)
	require.Equal(t, "CONFIRMED", claimed.Status)

	require.Equal(t, "CONFIRMED", wlBookingStatus(t, pg, offer.BookingID))
	require.Equal(t, "CLAIMED", wlEntryStatus(t, pg, offer.EntryID))

	// held_until is cleared: the row is a booking now, not an offer, and the
	// sweeper must not find it.
	var heldUntilSet bool
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT held_until IS NOT NULL FROM bookings WHERE id = $1`, offer.BookingID).Scan(&heldUntilSet))
	require.False(t, heldUntilSet)

	// A repeated claim converges rather than erroring: the student's intent is
	// already satisfied and a scary conflict for a successful action is worse
	// than a duplicate 200.
	again, err := r.svc.Claim(ctx, offer.BookingID, waiter)
	require.NoError(t, err)
	require.Equal(t, "CONFIRMED", again.Status)
}

// TestClaim_AfterExpiryRejected pins the deadline into the UPDATE's guard.
//
// The rig mints an already-expired offer (a negative claim window) rather than
// sleeping: the check lives in Postgres' now(), so a test that waited would be
// slower and no more convincing.
func TestClaim_AfterExpiryRejected(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, -time.Minute)

	users := pg.Users(t, 2)
	owner, waiter := users[0], users[1]
	start, end := wlSlot(15, time.Hour)

	r.join(t, waiter, start, end)
	offer := wlPromoteOne(t, r, r.book(t, owner, start, time.Hour), owner)

	_, err := r.svc.Claim(ctx, offer.BookingID, waiter)
	require.ErrorIs(t, err, booking.ErrOfferExpired)
	require.Equal(t, "HELD", wlBookingStatus(t, pg, offer.BookingID),
		"a rejected claim must not change the booking; the sweeper reclaims it")
}

func TestClaim_NotOwnerForbidden(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, 10*time.Minute)

	users := pg.Users(t, 3)
	owner, waiter, outsider := users[0], users[1], users[2]
	start, end := wlSlot(15, time.Hour)

	r.join(t, waiter, start, end)
	offer := wlPromoteOne(t, r, r.book(t, owner, start, time.Hour), owner)

	_, err := r.svc.Claim(ctx, offer.BookingID, outsider)
	require.ErrorIs(t, err, booking.ErrForbidden)

	// Including the student who cancelled: releasing a court does not entitle
	// you to take back the offer it produced.
	_, err = r.svc.Claim(ctx, offer.BookingID, owner)
	require.ErrorIs(t, err, booking.ErrForbidden)

	require.Equal(t, "HELD", wlBookingStatus(t, pg, offer.BookingID))
	require.Equal(t, "PROMOTED", wlEntryStatus(t, pg, offer.EntryID))
}

// ---------------------------------------------------------------------------
// Sweeper — §6.3
// ---------------------------------------------------------------------------

// TestSweeper_ExpiresAndPromotesNext walks the whole expiry cycle: an offer
// nobody claimed is reclaimed, its queue entry retires, and the freed window
// goes straight to the next student rather than sitting empty until somebody
// happens to refresh the grid.
func TestSweeper_ExpiresAndPromotesNext(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, -time.Minute) // offers are born expired

	users := pg.Users(t, 3)
	owner, first, second := users[0], users[1], users[2]
	start, end := wlSlot(15, time.Hour)

	e1 := r.join(t, first, start, end)
	e2 := r.join(t, second, start, end)

	offer := wlPromoteOne(t, r, r.book(t, owner, start, time.Hour), owner)
	require.Equal(t, e1.ID, offer.EntryID, "the head of the queue is offered first")

	res, err := r.queue.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.Expired)
	require.Equal(t, 1, res.Promoted)

	require.Equal(t, "CANCELLED", wlBookingStatus(t, pg, offer.BookingID),
		"an unclaimed hold must be released, or the court is lost for the day")
	require.Equal(t, "EXPIRED", wlEntryStatus(t, pg, e1.ID))
	require.Equal(t, "PROMOTED", wlEntryStatus(t, pg, e2.ID),
		"the freed window must go to the next in line in the same transaction")

	// And the new offer holds the same window the old one did.
	offers := wlOffers(t, pg)
	require.Len(t, offers, 2)
	for _, o := range offers {
		require.True(t, o.Start.Equal(start))
	}

	// An empty queue makes the next sweep a no-op rather than an error.
	res, err = r.queue.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.Expired)
	require.Equal(t, 0, res.Promoted, "nobody left to promote")
}

// TestSweeper_AndCancelDoNotDoublePromote is the reason the sweeper claims
// through waitlist_claim_head instead of growing its own "who is next" query.
//
// A sweeper reclaiming one window and a student cancelling another run at the
// same instant against the same queue. Two independent readers of the WAITING
// rows would both pick the head and hand one person two courts. Because both go
// through the same locking statement, they cannot — and because that statement
// skips locked rows, the sweeper's batch-long transaction does not hold the
// cancel up while it works.
func TestSweeper_AndCancelDoNotDoublePromote(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	cat := testutil.Catalogue(t, pg)
	court := testutil.CourtID()
	testutil.WarmCatalogue(t, cat, court)
	pg.Warm(t, 10)

	expiring := waitlist.NewService(pg.DB, cat, -time.Minute, wlQuiet())
	live := waitlist.NewService(pg.DB, cat, 10*time.Minute, wlQuiet())
	svc := pg.BookingServiceWith(t, cat)

	users := pg.Users(t, 6)
	owners, waiters := users[:2], users[2:]

	base, _ := wlSlot(15, time.Hour)
	firstHour := svcBook(t, svc, court, owners[0], base, time.Hour)
	secondHour := svcBook(t, svc, court, owners[1], base.Add(time.Hour), time.Hour)

	// Four waiters, one queue spanning both hours.
	for _, w := range waiters {
		_, err := live.Join(ctx, w, court, base, base.Add(2*time.Hour))
		require.NoError(t, err)
	}

	// Set the board: cancel the first hour with the expiring service, so its
	// promotion is already past its claim window and the sweeper has work.
	svc.WithPromotion(expiring)
	_, err := svc.Cancel(ctx, firstHour.ID, owners[0], "")
	require.NoError(t, err)
	require.Equal(t, 1, len(wlOffers(t, pg)))

	// Now race: the sweeper reclaims the first hour and promotes into it, while
	// a student cancels the second hour and promotes into that.
	svc.WithPromotion(live)
	out := testutil.Race(t, 2, func(ctx context.Context, i int) (any, error) {
		if i == 0 {
			return live.Sweep(ctx)
		}
		return svc.Cancel(ctx, secondHour.ID, owners[1], "")
	})
	require.Empty(t, out.Failures(), "%v", out.Failures())

	// Three offers have been made in total: the expired one, the sweeper's
	// replacement, and the cancel's. Every one went to a different student.
	offers := wlOffers(t, pg)
	require.Len(t, offers, 3)

	seen := map[uuid.UUID]bool{}
	for _, o := range offers {
		require.False(t, seen[o.UserID],
			"student %s was offered two courts — the sweeper and the cancel claimed the same row", o.UserID)
		seen[o.UserID] = true
	}

	// Exactly two courts are held right now: the expired offer's booking was
	// cancelled by the sweep.
	require.Equal(t, 2, wlHoldCount(t, pg))
}

// svcBook books through an arbitrary service, for the tests that need two
// differently-configured queues on one booking service.
func svcBook(t *testing.T, svc *booking.Service, court, user uuid.UUID, start time.Time, d time.Duration) *booking.Booking {
	t.Helper()
	b, err := svc.Create(context.Background(), booking.CreateRequest{
		FacilityID: court, UserID: user, Start: start,
		Duration: d, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)
	return b
}

// ---------------------------------------------------------------------------
// A cancel never fails because a promotion failed
// ---------------------------------------------------------------------------

// failingPromoter refuses without touching the transaction.
type failingPromoter struct{ err error }

func (f failingPromoter) Promote(context.Context, pgx.Tx, uuid.UUID, time.Time, time.Time) error {
	return f.err
}

// abortingPromoter reproduces what a 23P01 on the hold actually does to the
// transaction: it raises inside a savepoint, leaving the subtransaction
// aborted, then rolls back to the savepoint and reports the failure.
//
// This is the interesting case. Without the savepoint the cancelling
// transaction would be poisoned — every later statement fails with "current
// transaction is aborted" — and the student would be told their cancellation
// failed because somebody else's promotion did.
type abortingPromoter struct{}

func (abortingPromoter) Promote(ctx context.Context, tx pgx.Tx, _ uuid.UUID, _, _ time.Time) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	// Guaranteed to raise, and to abort the subtransaction with it.
	_, execErr := sp.Exec(ctx, `SELECT 1 / 0`)
	if err := sp.Rollback(ctx); err != nil {
		return err
	}
	return execErr
}

func TestCancelSucceedsWhenPromotionFails(t *testing.T) {
	start, end := wlSlot(15, time.Hour)

	cases := []struct {
		name     string
		promoter booking.Promoter
	}{
		{"promoter returns an error", failingPromoter{err: context.DeadlineExceeded}},
		{"promoter aborts and recovers a savepoint", abortingPromoter{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pg := testutil.Postgres(t)
			ctx := context.Background()
			r := newWLRig(t, pg, 10*time.Minute)

			users := pg.Users(t, 2)
			owner, waiter := users[0], users[1]

			b := r.book(t, owner, start, time.Hour)
			entry := r.join(t, waiter, start, end)

			r.svc.WithPromotion(tc.promoter)
			cancelled, err := r.svc.Cancel(ctx, b.ID, owner, "")
			require.NoError(t, err, "a cancel must never fail because a promotion failed")
			require.Equal(t, "CANCELLED", cancelled.Status)

			// The cancel's own side effects still committed.
			require.Equal(t, "CANCELLED", wlBookingStatus(t, pg, b.ID))
			var events int
			require.NoError(t, pg.Pool.QueryRow(ctx,
				`SELECT count(*)::int FROM booking_events WHERE booking_id = $1 AND to_status = 'CANCELLED'`,
				b.ID).Scan(&events))
			require.Equal(t, 1, events)

			// Nobody was promoted, and nothing is holding the court: the entry
			// is still queueing for the next cancel or the sweeper.
			require.Equal(t, "WAITING", wlEntryStatus(t, pg, entry.ID))
			require.Equal(t, 0, wlHoldCount(t, pg))
		})
	}
}

// ---------------------------------------------------------------------------
// Joining
// ---------------------------------------------------------------------------

// TestJoin_DuplicateRejected leaves the decision to uq_waitlist_live rather
// than to a SELECT that checked first. The read-then-write ban is not special
// to the booking path: two simultaneous joins would both find no existing entry
// and both insert.
func TestJoin_DuplicateRejected(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, 10*time.Minute)

	users := pg.Users(t, 2)
	start, end := wlSlot(15, time.Hour)

	first := r.join(t, users[0], start, end)

	_, err := r.queue.Join(ctx, users[0], r.court, start, end)
	require.ErrorIs(t, err, waitlist.ErrAlreadyWaiting)

	// Somebody else joining the same window is not a duplicate.
	_, err = r.queue.Join(ctx, users[1], r.court, start, end)
	require.NoError(t, err)

	// And leaving frees the key: the index only covers live entries.
	require.NoError(t, r.queue.Leave(ctx, first.ID, users[0]))
	rejoined, err := r.queue.Join(ctx, users[0], r.court, start, end)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, rejoined.ID)

	var live int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*)::int FROM waitlist WHERE status = 'WAITING'`).Scan(&live))
	require.Equal(t, 2, live)
}

// TestJoin_ReturnsCorrectPosition separates the two numbers that could be
// called "position": the bigserial ordering key, which is global and never
// computed, and the place a student reads, which is derived from the entries
// still waiting and moves up when somebody ahead leaves.
func TestJoin_ReturnsCorrectPosition(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, 10*time.Minute)

	users := pg.Users(t, 3)
	start, end := wlSlot(15, time.Hour)

	e1 := r.join(t, users[0], start, end)
	e2 := r.join(t, users[1], start, end)
	e3 := r.join(t, users[2], start, end)

	require.Equal(t, 1, e1.Place)
	require.Equal(t, 2, e2.Place)
	require.Equal(t, 3, e3.Place)

	// The ordering key ascends but is not the place; nothing incremented it.
	require.Less(t, e1.Position, e2.Position)
	require.Less(t, e2.Position, e3.Position)

	for want, e := range []*waitlist.Entry{e1, e2, e3} {
		place, err := r.queue.Position(ctx, e.ID)
		require.NoError(t, err)
		require.Equal(t, want+1, place)
	}

	// The head leaves; everyone behind moves up without a row being rewritten.
	require.NoError(t, r.queue.Leave(ctx, e1.ID, users[0]))

	place, err := r.queue.Position(ctx, e2.ID)
	require.NoError(t, err)
	require.Equal(t, 1, place)

	place, err = r.queue.Position(ctx, e3.ID)
	require.NoError(t, err)
	require.Equal(t, 2, place)

	// An entry that has left has no place at all.
	place, err = r.queue.Position(ctx, e1.ID)
	require.NoError(t, err)
	require.Equal(t, 0, place)

	_, err = r.queue.Position(ctx, uuid.New())
	require.ErrorIs(t, err, waitlist.ErrNotFound)

	// Somebody else's entry is not theirs to abandon.
	require.ErrorIs(t, r.queue.Leave(ctx, e2.ID, users[2]), waitlist.ErrForbidden)
}

// ---------------------------------------------------------------------------
// The common case
// ---------------------------------------------------------------------------

// TestEmptyWaitlist_CancelIsPlainCancel guards the path almost every cancel
// takes. Wiring the queue in must not change what happens when nobody is in it:
// no hold appears, and the slot is free for the next person to book — which is
// non-negotiable #4 still holding with the promotion hook attached.
func TestEmptyWaitlist_CancelIsPlainCancel(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newWLRig(t, pg, 10*time.Minute)

	users := pg.Users(t, 2)
	owner, other := users[0], users[1]
	start, _ := wlSlot(15, time.Hour)

	b := r.book(t, owner, start, time.Hour)

	cancelled, err := r.svc.Cancel(ctx, b.ID, owner, "changed my mind")
	require.NoError(t, err)
	require.Equal(t, "CANCELLED", cancelled.Status)
	require.Equal(t, 0, wlHoldCount(t, pg), "an empty queue must not produce a hold")

	var entries int
	require.NoError(t, pg.Pool.QueryRow(ctx, `SELECT count(*)::int FROM waitlist`).Scan(&entries))
	require.Equal(t, 0, entries)

	// The slot is genuinely free, not held for nobody.
	_, err = r.svc.Create(ctx, booking.CreateRequest{
		FacilityID: r.court, UserID: other, Start: start,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)
}

// TestJoin_RefusesSharedFacility documents the one queue that does not exist.
//
// A HELD row reserves nothing on the gymnasium — its occupancy is the
// slot_capacity counter, not the exclusion constraint — so an offer there could
// not hold the place it promised. A queue that can never promote is worse than
// no queue, because the student stops looking elsewhere.
func TestJoin_RefusesSharedFacility(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	cat := testutil.Catalogue(t, pg)
	queue := waitlist.NewService(pg.DB, cat, 10*time.Minute, wlQuiet())

	start, end := wlSlot(15, time.Hour)
	_, err := queue.Join(ctx, testutil.StudentID(0), testutil.GymID(), start, end)
	require.ErrorIs(t, err, waitlist.ErrValidation)

	_, err = queue.Join(ctx, testutil.StudentID(0), uuid.New(), start, end)
	require.ErrorIs(t, err, waitlist.ErrNotFound)
}
