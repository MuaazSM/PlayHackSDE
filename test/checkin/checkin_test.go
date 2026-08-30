package checkin_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/checkin"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestCheckIn_MarksAttendance is the happy path: the student scans the code on
// the wall and a row appears in check_ins.
//
// That row is the whole point. It is not a flag on the booking — attendance is a
// fact recorded in one table, so there is no second field that could disagree
// with it — and it is what the no-show sweep's NOT EXISTS consults an hour later.
func TestCheckIn_MarksAttendance(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg).openCheckIn()

	student := pg.Users(t, 1)[0]
	start, _ := ciSlot(15, time.Hour)
	b := r.book(t, r.court, student, start, time.Hour)

	att, err := r.svc.Redeem(ctx, b.ID, student, r.token(r.court))
	require.NoError(t, err)
	require.False(t, att.Converged)
	require.Equal(t, b.ID, att.BookingID)
	require.Equal(t, booking.Reference(b.ID), att.Reference)
	require.Equal(t, r.court, att.FacilityID)

	row, found := ciCheckIn(t, pg, b.ID)
	require.True(t, found, "attendance must be a row, not a return value")
	require.Equal(t, "QR", row.Method)
	require.NotNil(t, row.TokenID)

	// The token is NEVER stored whole. It is a keyed hash of the minute, so a
	// stored copy would be a credential that is worthless in two minutes and
	// embarrassing in the meantime; a short prefix is enough to correlate two
	// scans of the same displayed code.
	require.NotEqual(t, r.token(r.court), *row.TokenID)
	require.Less(t, len(*row.TokenID), len(r.token(r.court)))

	// Checking in changes no occupancy — the same student still holds the same
	// court — so the booking is untouched and nothing was published.
	require.Equal(t, "CONFIRMED", ciBookingStatus(t, pg, b.ID))
	require.Equal(t, 0, ciOutboxCount(t, pg, "booking.no_show"))
}

// TestCheckIn_Idempotent: twice is once. The PRIMARY KEY on
// check_ins(booking_id) makes a second scan physically unable to write a second
// row, so both calls answer 200 and the second returns the original record.
//
// This is a STRONGER guarantee than a client-supplied Idempotency-Key, which is
// why the endpoint does not require one: it holds across devices and across
// clients that forget to send a header. A student scanning again on a friend's
// phone converges rather than double-recording.
func TestCheckIn_Idempotent(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg).openCheckIn()

	student := pg.Users(t, 1)[0]
	start, _ := ciSlot(15, time.Hour)
	b := r.book(t, r.court, student, start, time.Hour)

	first, err := r.svc.Redeem(ctx, b.ID, student, r.token(r.court))
	require.NoError(t, err)
	require.False(t, first.Converged)

	second, err := r.svc.Redeem(ctx, b.ID, student, r.token(r.court))
	require.NoError(t, err, "a repeated scan is a satisfied retry, not a conflict")
	require.True(t, second.Converged)

	// The SAME record, not a fresh one. A second `at` would mean a second row had
	// been written and the first silently lost.
	require.True(t, first.At.Equal(second.At),
		"the retry must return the original attendance time, got %s then %s", first.At, second.At)
	require.Equal(t, 1, ciCheckInCount(t, pg))
}

// TestCheckIn_BeforeWindowRejected uses the PRODUCTION window: ten minutes.
//
// A student on the bus cannot check in for a slot tomorrow. If they could,
// "checked in" would stop meaning "at the venue" and the no-show release — the
// entire point of the feature — would never fire for anybody who remembered to
// tap the button.
func TestCheckIn_BeforeWindowRejected(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg) // real window: [start - 10m, start + 15m]

	student := pg.Users(t, 1)[0]
	start, _ := ciSlot(15, time.Hour)
	b := r.book(t, r.court, student, start, time.Hour)

	_, err := r.svc.Redeem(ctx, b.ID, student, r.token(r.court))
	require.ErrorIs(t, err, checkin.ErrOutsideWindow)

	require.Equal(t, 0, ciCheckInCount(t, pg), "a rejected check-in must write nothing")
	require.Equal(t, "CONFIRMED", ciBookingStatus(t, pg, b.ID))
}

// TestCheckIn_AfterGraceRejected closes the far edge.
//
// Past the grace period the court is on its way to being released to somebody
// else; accepting a late check-in would let a student who never arrived reclaim
// a slot the sweeper is about to hand to the next person in the queue.
//
// The window is moved rather than the clock, because the comparison happens in
// Postgres' now() and a Go clock would not reach it.
func TestCheckIn_AfterGraceRejected(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg).closeGrace()

	student := pg.Users(t, 1)[0]
	start, _ := ciSlot(15, time.Hour)
	b := r.book(t, r.court, student, start, time.Hour)

	_, err := r.svc.Redeem(ctx, b.ID, student, r.token(r.court))
	require.ErrorIs(t, err, checkin.ErrOutsideWindow)
	require.Equal(t, 0, ciCheckInCount(t, pg))
}

// TestCheckIn_NotOwnerForbidden: a valid token is proof of being at the venue,
// not proof of holding the booking. Both are required.
//
// The token is deliberately the RIGHT one here, so the rejection can only be
// coming from the ownership check — a test that passed a bad token as well would
// pass even if ownership were never verified.
func TestCheckIn_NotOwnerForbidden(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg).openCheckIn()

	users := pg.Users(t, 2)
	owner, outsider := users[0], users[1]
	start, _ := ciSlot(15, time.Hour)
	b := r.book(t, r.court, owner, start, time.Hour)

	_, err := r.svc.Redeem(ctx, b.ID, outsider, r.token(r.court))
	require.ErrorIs(t, err, booking.ErrForbidden)
	require.Equal(t, 0, ciCheckInCount(t, pg))

	// The owner, with the same token, gets in.
	_, err = r.svc.Redeem(ctx, b.ID, owner, r.token(r.court))
	require.NoError(t, err)
	require.Equal(t, 1, ciCheckInCount(t, pg))
}

// TestCheckIn_WrongVenueTokenRejected is TestQRToken_WrongFacilityRejected
// carried through the service: the token is verified against the BOOKING's
// facility, which is the only binding that makes the code mean anything.
//
// Without it a student could stand at the gym, scan the gym's screen, and check
// into the tennis court they booked and are not at.
func TestCheckIn_WrongVenueTokenRejected(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg).openCheckIn()

	student := pg.Users(t, 1)[0]
	start, _ := ciSlot(15, time.Hour)
	b := r.book(t, r.court, student, start, time.Hour)

	_, err := r.svc.Redeem(ctx, b.ID, student, r.token(r.gym))
	require.ErrorIs(t, err, checkin.ErrInvalidToken)
	require.Equal(t, 0, ciCheckInCount(t, pg))
}

// TestCheckIn_UnknownBookingNotFound keeps 404 distinct from 403: a booking that
// does not exist is not "somebody else's".
func TestCheckIn_UnknownBookingNotFound(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg).openCheckIn()

	_, err := r.svc.Redeem(ctx, uuid.New(), pg.Users(t, 1)[0], r.token(r.court))
	require.ErrorIs(t, err, booking.ErrNotFound)
}

// TestCheckIn_CancelledBookingNotCheckable stops a released court being
// reclaimed by a scan. The booking no longer occupies the slot — somebody else
// may already have it — so attendance against it would be a claim about a court
// this student does not hold.
func TestCheckIn_CancelledBookingNotCheckable(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()
	r := newCIRig(t, pg).openCheckIn()

	student := pg.Users(t, 1)[0]
	start, _ := ciSlot(15, time.Hour)
	b := r.book(t, r.court, student, start, time.Hour)

	_, err := r.bookings.Cancel(ctx, b.ID, student, "changed my mind")
	require.NoError(t, err)

	_, err = r.svc.Redeem(ctx, b.ID, student, r.token(r.court))
	require.ErrorIs(t, err, checkin.ErrNotCheckable)
	require.Equal(t, 0, ciCheckInCount(t, pg))
}
