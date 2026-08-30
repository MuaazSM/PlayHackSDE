// Package checkin_test covers IMPLEMENTATION.md §7: the venue QR token, the
// redemption that records attendance, and the sweep that releases a court
// nobody turned up for and hands it to the queue.
//
// Everything below the token tests runs against a real Postgres. The check-in
// window and the no-show deadline are both evaluated in Postgres' now(), by
// design — a claim about time that a Go test could satisfy by moving a Go clock
// would not be testing the thing that actually decides.
package checkin_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/checkin"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/waitlist"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// testSecret is the CHECKIN_HMAC_SECRET these tests sign with. The rig's minter
// and the service's minter are the SAME object, exactly as they are in cmd/api:
// a test that signed with a second minter would pass even if the service had
// been wired to the wrong secret.
const testSecret = "test-checkin-secret"

// ciQuiet keeps the sweep log out of the test output.
func ciQuiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ciSlot returns [start, end) at TOMORROW's given IST hour, in UTC, and
// ciSlotPast the same hour YESTERDAY.
//
// Never "an hour from now". The seeded courts open 06:00-22:00 IST, so a slot
// anchored to the wall clock is outside opening hours for a third of the day and
// this suite runs unattended overnight. A fixed mid-afternoon hour on a fixed day
// is inside them whatever time it is.
func ciSlot(hour int, d time.Duration) (start, end time.Time) { return ciSlotOn(1, hour, d) }

// ciSlotPast is a window that has already closed — the shape the no-show sweep
// exists for.
//
// Yesterday rather than "twenty minutes ago" for the same opening-hours reason.
// The sweep compares lower(during) against now() and nothing else, so a window
// that closed yesterday and one that started twenty minutes ago take an
// identical path through it; only the second depends on what time the suite runs.
func ciSlotPast(hour int, d time.Duration) (start, end time.Time) { return ciSlotOn(-1, hour, d) }

func ciSlotOn(dayOffset, hour int, d time.Duration) (start, end time.Time) {
	day := time.Now().In(testutil.IST).AddDate(0, 0, dayOffset)
	start = time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, testutil.IST).UTC()
	return start, start.Add(d)
}

// Check-in windows for tests.
//
// The window is evaluated in Postgres' now(), so these tests do not move a
// clock — a Go clock would not reach the comparison that decides. They move the
// WINDOW instead, which is the same trick the waitlist suite uses when it mints
// already-expired offers with a negative claim TTL:
//
//   - openEarly pulls the NEAR edge far enough back that a booking scheduled for
//     tomorrow is already checkable.
//   - lateGrace pushes the FAR edge far enough forward that a booking from
//     yesterday still is.
//   - closedGrace puts the far edge BEFORE now, so a booking is already past its
//     grace period however far in the future it is.
//
// 72 hours rather than 24 because "tomorrow at 15:00" is anywhere from 13 to 37
// hours away depending on when the suite runs, and a bound that depends on the
// wall clock is a flake waiting for a night run.
//
// The no-show sweep needs none of this: it runs against the REAL grace period,
// because a booking from yesterday is past a fifteen-minute deadline by any
// clock.
const (
	openEarly   = 72 * time.Hour
	lateGrace   = 72 * time.Hour
	closedGrace = -48 * time.Hour
)

// ciRig is one test's worth of wiring.
type ciRig struct {
	pg       *testutil.PG
	cat      *facility.Repo
	minter   *checkin.Minter
	queue    *waitlist.Service
	bookings *booking.Service

	// svc redeems. Its window is [start - early, start + grace].
	svc *checkin.Service

	// sweeper releases no-shows, on the REAL grace period. Split from svc only so
	// a test can widen the check-in window without also widening the deadline the
	// sweep is meant to be enforcing.
	sweeper *checkin.Service

	court uuid.UUID
	gym   uuid.UUID
}

// newCIRig builds the rig with the PRODUCTION check-in window: ten minutes
// before the slot, fifteen after. Tests that need to be inside it call
// openCheckIn.
func newCIRig(t *testing.T, pg *testutil.PG) *ciRig {
	t.Helper()

	cat := testutil.Catalogue(t, pg)
	court, gym := testutil.CourtID(), testutil.GymID()
	testutil.WarmCatalogue(t, cat, court, gym)

	minter := checkin.NewMinter(testSecret)
	queue := waitlist.NewService(pg.DB, cat, 10*time.Minute, ciQuiet())

	return &ciRig{
		pg:       pg,
		cat:      cat,
		minter:   minter,
		queue:    queue,
		bookings: pg.BookingServiceWith(t, cat).WithPromotion(queue),
		svc:      checkin.NewService(pg.DB, cat, minter, checkin.DefaultGracePeriod, ciQuiet()),
		sweeper: checkin.NewService(pg.DB, cat, minter, checkin.DefaultGracePeriod, ciQuiet()).
			WithPromotion(queue),
		court: court,
		gym:   gym,
	}
}

// openCheckIn widens the near edge of the window so a booking scheduled for
// tomorrow is already checkable. The far edge stays at the real grace period.
func (r *ciRig) openCheckIn() *ciRig {
	r.svc.WithWindow(openEarly, checkin.DefaultGracePeriod)
	return r
}

// lateCheckIn widens the FAR edge, so a booking whose window has already closed
// can still be marked as attended. Used to set the board for the sweep tests:
// somebody who did turn up, to a slot that is now over.
func (r *ciRig) lateCheckIn() *ciRig {
	r.svc.WithWindow(0, lateGrace)
	return r
}

// closeGrace puts the whole window in the past, so the same booking is already
// past its grace period.
func (r *ciRig) closeGrace() *ciRig {
	r.svc.WithWindow(openEarly, closedGrace)
	return r
}

// noPromotion detaches the queue from the sweeper, for the tests that are about
// the release itself.
func (r *ciRig) noPromotion() *ciRig {
	r.sweeper.WithPromotion(nil)
	return r
}

func (r *ciRig) book(t *testing.T, facilityID, user uuid.UUID, start time.Time, d time.Duration) *booking.Booking {
	t.Helper()
	b, err := r.bookings.Create(context.Background(), booking.CreateRequest{
		FacilityID: facilityID, UserID: user, Start: start,
		Duration: d, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)
	return b
}

// bookPast creates a booking whose window has already closed.
//
// It moves the booking SERVICE's clock, not the database's, and only for the
// duration of the insert. That matters: the row lands through the real write
// path — Mechanism A's exclusion constraint, or Mechanism B's counter row for
// the gym — so the sweep is later releasing something that was genuinely booked,
// counters and all, rather than a row a test hand-wrote into the table.
//
// The one check being bypassed is booking.Create's "start is in the future",
// which is a validation of intent and has nothing to do with what the sweep
// does. Opening hours are still enforced, and are unaffected: they are compared
// against the facility's LOCAL day, not against now.
func (r *ciRig) bookPast(t *testing.T, facilityID, user uuid.UUID, start time.Time, d time.Duration) *booking.Booking {
	t.Helper()
	b, err := r.tryBookPast(facilityID, user, start, d)
	require.NoError(t, err)
	return b
}

// tryBookPast is bookPast for the cases that EXPECT the write to be refused —
// where the point of the test is which error comes back.
func (r *ciRig) tryBookPast(facilityID, user uuid.UUID, start time.Time, d time.Duration) (*booking.Booking, error) {
	r.bookings.WithClock(func() time.Time { return start.Add(-time.Hour) })
	defer r.bookings.WithClock(time.Now)
	return r.bookings.Create(context.Background(), booking.CreateRequest{
		FacilityID: facilityID, UserID: user, Start: start,
		Duration: d, IdemKey: uuid.NewString(),
	})
}

// token is the code the venue display is showing for this facility right now.
func (r *ciRig) token(facilityID uuid.UUID) string {
	return r.minter.Mint(facilityID)
}

// ---------------------------------------------------------------------------
// Assertions read the DATABASE, not the return value. The guarantee under test
// is a property of the rows; a test that trusted what the service said would
// pass even if nothing had been written.
// ---------------------------------------------------------------------------

func ciBookingStatus(t *testing.T, pg *testutil.PG, id uuid.UUID) string {
	t.Helper()
	var s string
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT status::text FROM bookings WHERE id = $1`, id).Scan(&s))
	return s
}

func ciCheckInCount(t *testing.T, pg *testutil.PG) int {
	t.Helper()
	var n int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM check_ins`).Scan(&n))
	return n
}

type ciAttendance struct {
	At      time.Time
	Method  string
	TokenID *string
}

// ciCheckIn reads the attendance row for a booking, or reports absence.
func ciCheckIn(t *testing.T, pg *testutil.PG, id uuid.UUID) (ciAttendance, bool) {
	t.Helper()
	var a ciAttendance
	err := pg.Pool.QueryRow(context.Background(),
		`SELECT at, method, token_id FROM check_ins WHERE booking_id = $1`, id).
		Scan(&a.At, &a.Method, &a.TokenID)
	if err != nil {
		return a, false
	}
	return a, true
}

// ciEventCount counts audit rows for one booking's transition to a status.
func ciEventCount(t *testing.T, pg *testutil.PG, id uuid.UUID, to string) int {
	t.Helper()
	var n int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM booking_events
		  WHERE booking_id = $1 AND to_status = $2::booking_status`, id, to).Scan(&n))
	return n
}

// ciOutboxCount counts pending side effects on a topic.
func ciOutboxCount(t *testing.T, pg *testutil.PG, topic string) int {
	t.Helper()
	var n int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM outbox WHERE topic = $1`, topic).Scan(&n))
	return n
}

// ciBooked reads a shared facility's capacity counter for a slot.
func ciBooked(t *testing.T, pg *testutil.PG, facilityID uuid.UUID, slotStart time.Time) int {
	t.Helper()
	var n int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT booked FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
		facilityID, slotStart).Scan(&n))
	return n
}

// ciWaitlistStatus reads one queue entry's status.
func ciWaitlistStatus(t *testing.T, pg *testutil.PG, id uuid.UUID) string {
	t.Helper()
	var s string
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT status::text FROM waitlist WHERE id = $1`, id).Scan(&s))
	return s
}
