package closures_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestClosure_BlocksBookingOnCourt is the easy half: an exclusive facility.
//
// Nothing new does the blocking here. The BLOCKED row is inside
// no_double_book's predicate, so a student's INSERT loses to it exactly as it
// would lose to another student — same 23P01, same 409, same code path. That is
// the whole design claim of §10.4, and this test is what makes it checkable.
func TestClosure_BlocksBookingOnCourt(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)

	c := r.close(t, r.court, start, end, "court resurfacing")
	require.NotEqual(t, uuid.Nil, c.ID)
	require.Equal(t, "BLOCKED", statusOf(t, r.pg, c.ID))

	_, err := r.book(r.court, testutil.StudentID(0), start, time.Hour)
	require.Error(t, err)
	require.ErrorIs(t, err, booking.ErrSlotTaken)

	// One row holds the window, and it is the closure. Read from the table
	// rather than from the error: the constraint is the thing under test.
	require.Equal(t, 1, overlapping(t, r.pg, r.court, start, end))

	// A window the closure does not cover is untouched — a closure closes what it
	// says it closes, not the day.
	next, nextEnd := slot(20, time.Hour)
	_, err = r.book(r.court, testutil.StudentID(1), next, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, overlapping(t, r.pg, r.court, next, nextEnd))

	require.Equal(t, 1, outboxCount(t, r.pg, outbox.TopicClosureCreated),
		"closure.created goes through the outbox, inside the transaction")
}

// TestClosure_BlocksBookingOnGym is the case this whole feature turns on.
//
// A BLOCKED row on a SHARED facility is NOT in no_double_book's index — the
// predicate is scoped to is_exclusive so that Mechanism B can exist at all — so
// the row alone blocks nothing. Zeroing slot_capacity is what makes the closure
// real: Mechanism B's `booked < capacity` guard then fails trivially and
// capacity_take returns zero rows.
//
// Delete the zeroing step from CreateClosure and this test fails while every
// other test in the file still passes. That asymmetry is the point.
func TestClosure_BlocksBookingOnGym(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)

	r.close(t, r.gym, start, end, "deep clean")

	capacity, booked, found := capacityOf(t, r.pg, r.gym, start)
	require.True(t, found, "the closure must materialise the counter row it closes")
	require.Equal(t, 0, capacity, "capacity must be forced to 0 for the closed slot")
	require.Equal(t, 0, booked)

	// The gym has capacity 30 and nobody in it. Without the step above this
	// request succeeds and the closure is decorative.
	_, err := r.book(r.gym, testutil.StudentID(0), start, time.Hour)
	require.Error(t, err)
	require.ErrorIs(t, err, booking.ErrCapacityFull)

	_, booked, _ = capacityOf(t, r.pg, r.gym, start)
	require.Equal(t, 0, booked, "a refused booking must not have taken a place")

	// Control: the very next slot is not closed, so Mechanism B still admits
	// bookings there. Without this the test would also pass if closures broke the
	// gym outright.
	next, _ := slot(20, time.Hour)
	_, err = r.book(r.gym, testutil.StudentID(1), next, time.Hour)
	require.NoError(t, err)

	capacity, booked, _ = capacityOf(t, r.pg, r.gym, next)
	require.Equal(t, 30, capacity)
	require.Equal(t, 1, booked)

	require.Equal(t, 1, outboxCount(t, r.pg, outbox.TopicClosureCreated))
}

// TestClosure_BlocksBookingOnGym_MultiSlot closes a window spanning several grid
// blocks and an unaligned edge.
//
// A manager typing "17:30 to 20:00 for a burst pipe" means the 17:00, 18:00 and
// 19:00 blocks are gone. Snapping the counter rows to the closure's own start
// would create rows at 17:30 and 18:30 that capacity_take never reads, leaving
// the gym bookable behind a grid that says closed — the same silent failure as
// forgetting the step entirely, arrived at from the other direction.
func TestClosure_BlocksBookingOnGym_MultiSlot(t *testing.T) {
	r := newRig(t)

	start, _ := slot(17, 30*time.Minute)
	start = start.Add(30 * time.Minute) // 17:30 IST
	end, _ := slot(20, time.Hour)

	r.close(t, r.gym, start, end, "burst pipe")

	for _, hour := range []int{17, 18, 19} {
		blockStart, _ := slot(hour, time.Hour)
		capacity, _, found := capacityOf(t, r.pg, r.gym, blockStart)
		require.True(t, found, "block %02d:00 must have a counter row", hour)
		require.Equal(t, 0, capacity, "block %02d:00 must be closed", hour)

		_, err := r.book(r.gym, testutil.StudentID(hour), blockStart, time.Hour)
		require.ErrorIs(t, err, booking.ErrCapacityFull, "block %02d:00 must refuse bookings", hour)
	}

	// 20:00 is outside the window: the closure ends there and bounds are '[)'.
	after, _ := slot(20, time.Hour)
	_, err := r.book(r.gym, testutil.StudentID(0), after, time.Hour)
	require.NoError(t, err, "the block starting exactly at the closure's end is not closed")
}

// TestClosure_ShowsAsClosedInAvailability checks the read path agrees, for BOTH
// mechanisms.
//
// Availability is derived from the bookings table at read time (non-negotiable
// #4), so there is no flag to set — but the two facility kinds reach 'closed' by
// different routes: the exclusive query reads the BLOCKED row's status, and the
// shared one reads the row as an override on top of its counter. Both are
// asserted because a change to either query could break one and leave the other
// looking fine.
func TestClosure_ShowsAsClosedInAvailability(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)
	free, _ := slot(20, time.Hour)

	t.Run("exclusive", func(t *testing.T) {
		r.close(t, r.court, start, end, "resurfacing")
		require.Equal(t, facility.StateClosed, stateAt(t, r, r.court, start))
		require.Equal(t, facility.StateFree, stateAt(t, r, r.court, free))
	})

	t.Run("shared", func(t *testing.T) {
		r.close(t, r.gym, start, end, "deep clean")
		require.Equal(t, facility.StateClosed, stateAt(t, r, r.gym, start))
		require.Equal(t, facility.StateFree, stateAt(t, r, r.gym, free))
	})
}

// TestClosure_ConflictsWithExistingBooking409 covers the exclusive facility whose
// window is already taken.
//
// The closure is refused by the constraint itself — no application check decides
// this — and the manager gets the list of who is in the way, because "conflict"
// with no names is not something a human can act on.
func TestClosure_ConflictsWithExistingBooking409(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)

	b := r.mustBook(t, r.court, testutil.StudentID(0), start, time.Hour)

	_, err := r.tryClose(r.court, start, end, "resurfacing")
	require.Error(t, err)
	require.ErrorIs(t, err, booking.ErrSlotTaken, "the exclusion constraint refused the closure")

	var conflict *booking.ClosureConflict
	require.ErrorAs(t, err, &conflict)
	require.Len(t, conflict.Bookings, 1)
	require.Equal(t, b.ID, conflict.Bookings[0].ID)
	require.Equal(t, "student01", conflict.Bookings[0].RollNo)
	require.Equal(t, "Tennis Court 1", conflict.FacilityName)

	// The refused closure left nothing behind: still exactly one row on the
	// window, and it is the student's booking.
	require.Equal(t, 1, overlapping(t, r.pg, r.court, start, end))
	require.Equal(t, "CONFIRMED", statusOf(t, r.pg, b.ID))
	require.Equal(t, 0, outboxCount(t, r.pg, outbox.TopicClosureCreated))

	// And over HTTP, the same refusal carries the list in the one error envelope.
	token := r.login(t, "manager01")
	resp := r.do(t, http.MethodPost, "/api/v1/closures", token, map[string]any{
		"facility_id": r.court.String(),
		"start":       start.Format(time.RFC3339),
		"end":         end.Format(time.RFC3339),
		"reason":      "resurfacing",
	})
	require.Equal(t, http.StatusConflict, resp.status, "body: %s", resp.raw)

	body := resp.errorBody(t)
	require.Equal(t, "SLOT_TAKEN", body.Error)
	require.Len(t, body.Conflicts, 1)
	require.Equal(t, b.ID.String(), body.Conflicts[0].BookingID)
	require.Equal(t, "student01", body.Conflicts[0].RollNo)
}

// TestClosure_ListsAffectedBookings is the shared-facility counterpart: the
// closure SUCCEEDS with bookings inside it, because a closure and a booking can
// coexist on a facility whose occupancy is a counter rather than a constraint.
// The bookings are handed back for staff review.
func TestClosure_ListsAffectedBookings(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)

	for i := 0; i < 3; i++ {
		r.mustBook(t, r.gym, testutil.StudentID(i), start, time.Hour)
	}

	c := r.close(t, r.gym, start, end, "burst pipe")
	require.Len(t, c.Affected, 3)

	rolls := map[string]bool{}
	for _, a := range c.Affected {
		rolls[a.RollNo] = true
		require.Equal(t, "CONFIRMED", a.Status)
		require.NotEmpty(t, a.Name)
	}
	require.Equal(t, map[string]bool{"student01": true, "student02": true, "student03": true}, rolls)

	// The slot is closed even though three people are in it: capacity 0 with
	// booked 3 is a legitimate transient state, and migration 0009 admits exactly
	// it. Refusing to close a slot BECAUSE it has bookings would be the wrong
	// answer — a flooded gym is closed whether or not anybody booked it.
	capacity, count, _ := capacityOf(t, r.pg, r.gym, start)
	require.Equal(t, 0, capacity)
	require.Equal(t, 3, count)

	_, err := r.book(r.gym, testutil.StudentID(5), start, time.Hour)
	require.ErrorIs(t, err, booking.ErrCapacityFull, "no fourth student may join a closed slot")

	// An exclusive closure over an empty window affects nobody, and says so with
	// an empty list rather than a null.
	courtStart, courtEnd := slot(19, time.Hour)
	require.NotNil(t, r.close(t, r.court, courtStart, courtEnd, "resurfacing").Affected)
	require.Empty(t, r.close(t, r.court, courtStart, courtEnd, "resurfacing").Affected)
}

// TestClosure_DoesNotAutoCancel: the affected bookings are FLAGGED, not revoked.
//
// Cancelling somebody's court is a decision with a person on the other end of it,
// so it goes through the ordinary cancel path where they are notified, the
// counter is released and the queue is promoted. A closure transaction that did
// it silently would do all of that behind the manager's back.
func TestClosure_DoesNotAutoCancel(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)

	var ids []*booking.Booking
	for i := 0; i < 3; i++ {
		ids = append(ids, r.mustBook(t, r.gym, testutil.StudentID(i), start, time.Hour))
	}

	r.close(t, r.gym, start, end, "burst pipe")

	for _, b := range ids {
		require.Equal(t, "CONFIRMED", statusOf(t, r.pg, b.ID),
			"booking %s must survive the closure untouched", b.Reference)
	}

	// The counter still accounts for all three. A closure that quietly released
	// their places would show up here as booked = 0.
	_, count, _ := capacityOf(t, r.pg, r.gym, start)
	require.Equal(t, 3, count)

	// No cancellation was recorded, in the audit trail or in the outbox.
	require.Equal(t, 0, outboxCount(t, r.pg, outbox.TopicBookingCancelled))

	var events int
	require.NoError(t, r.pg.Pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM booking_events WHERE to_status = 'CANCELLED'`).Scan(&events))
	require.Equal(t, 0, events)
}

// TestClosure_RequiresManagerRole: the route is MANAGER-only, enforced by
// middleware over the real router.
//
// A student who could close a court could deny it to everybody else, which is
// the same damage as booking it forever and cheaper to do.
func TestClosure_RequiresManagerRole(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)

	student := r.login(t, "student01")
	manager := r.login(t, "manager01")

	body := map[string]any{
		"facility_id": r.court.String(),
		"start":       start.Format(time.RFC3339),
		"end":         end.Format(time.RFC3339),
		"reason":      "not your call",
	}

	resp := r.do(t, http.MethodPost, "/api/v1/closures", student, body)
	require.Equal(t, http.StatusForbidden, resp.status, "body: %s", resp.raw)
	require.Equal(t, "FORBIDDEN", resp.errorBody(t).Error)

	// Nothing was written. A 403 that still closed the court would be worse than
	// no check at all.
	require.Equal(t, 0, overlapping(t, r.pg, r.court, start, end))

	// The console's read is manager-only too: it names the students a closure
	// affects.
	require.Equal(t, http.StatusForbidden,
		r.do(t, http.MethodGet, "/api/v1/closures", student, nil).status)

	// Unauthenticated is 401, not 403 — a different fact about the caller.
	require.Equal(t, http.StatusUnauthorized,
		r.do(t, http.MethodPost, "/api/v1/closures", "", body).status)

	// The same request from the manager works, which is what makes the 403 above
	// a role check rather than a broken route.
	resp = r.do(t, http.MethodPost, "/api/v1/closures", manager, body)
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.raw)
	require.Equal(t, 1, overlapping(t, r.pg, r.court, start, end))

	// And a student may not withdraw one either.
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(resp.raw, &created))
	require.Equal(t, http.StatusForbidden,
		r.do(t, http.MethodDelete, "/api/v1/closures/"+created.ID, student, nil).status)
	require.Equal(t, "BLOCKED", statusOf(t, r.pg, uuid.MustParse(created.ID)))
}

// TestClosure_ReopenRestoresCapacity: withdrawing a gym closure puts the counter
// back to the facility's declared capacity.
//
// Restoring to the FACILITY's capacity, not to a number the closure remembered:
// the declared value is the only authority, and a stashed copy would be a second
// one to drift.
func TestClosure_ReopenRestoresCapacity(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)

	// One student is already in, so the restoration has to preserve `booked`
	// rather than reset the row.
	r.mustBook(t, r.gym, testutil.StudentID(0), start, time.Hour)

	c := r.close(t, r.gym, start, end, "deep clean")
	capacity, count, _ := capacityOf(t, r.pg, r.gym, start)
	require.Equal(t, 0, capacity)
	require.Equal(t, 1, count)

	reopened := r.reopen(t, c.ID)
	require.Equal(t, "CANCELLED", reopened.Status)
	require.Equal(t, "CANCELLED", statusOf(t, r.pg, c.ID),
		"the closure is cancelled, not deleted — the audit trail survives")

	capacity, count, _ = capacityOf(t, r.pg, r.gym, start)
	require.Equal(t, 30, capacity, "capacity is restored from the facility's declared value")
	require.Equal(t, 1, count, "the student who was already booked keeps their place")

	// Reopening again converges rather than double-restoring or erroring: the
	// status-guarded UPDATE matches nothing the second time.
	again, err := r.svc.Reopen(context.Background(), c.ID, r.manager(), "again")
	require.NoError(t, err)
	require.True(t, again.Converged)

	capacity, count, _ = capacityOf(t, r.pg, r.gym, start)
	require.Equal(t, 30, capacity)
	require.Equal(t, 1, count)
}

// TestClosure_ReopenMakesSlotBookableAgain closes the loop for both mechanisms.
//
// For the exclusive facility there is no release step at all: the row moving to
// CANCELLED drops it out of no_double_book's partial index, and the window is
// bookable the instant the transaction commits. For the shared one the counter
// has to be restored, because Mechanism B keeps a number rather than deriving
// from the rows.
func TestClosure_ReopenMakesSlotBookableAgain(t *testing.T) {
	t.Run("exclusive", func(t *testing.T) {
		r := newRig(t)
		start, end := slot(18, time.Hour)

		c := r.close(t, r.court, start, end, "resurfacing")
		_, err := r.book(r.court, testutil.StudentID(0), start, time.Hour)
		require.ErrorIs(t, err, booking.ErrSlotTaken)

		r.reopen(t, c.ID)
		require.Equal(t, facility.StateFree, stateAt(t, r, r.court, start))

		b, err := r.book(r.court, testutil.StudentID(0), start, time.Hour)
		require.NoError(t, err)
		require.Equal(t, "CONFIRMED", statusOf(t, r.pg, b.ID))
		require.Equal(t, 1, overlapping(t, r.pg, r.court, start, end))
	})

	t.Run("shared", func(t *testing.T) {
		r := newRig(t)
		start, end := slot(18, time.Hour)

		c := r.close(t, r.gym, start, end, "deep clean")
		_, err := r.book(r.gym, testutil.StudentID(0), start, time.Hour)
		require.ErrorIs(t, err, booking.ErrCapacityFull)

		r.reopen(t, c.ID)
		require.Equal(t, facility.StateFree, stateAt(t, r, r.gym, start))

		b, err := r.book(r.gym, testutil.StudentID(0), start, time.Hour)
		require.NoError(t, err)
		require.Equal(t, "CONFIRMED", statusOf(t, r.pg, b.ID))

		capacity, count, _ := capacityOf(t, r.pg, r.gym, start)
		require.Equal(t, 30, capacity)
		require.Equal(t, 1, count)
	})
}

// TestClosure_ReopenLeavesOverlappingClosureStanding: two managers close
// overlapping windows for different reasons; withdrawing one must not reopen the
// facility while the other still stands.
//
// The exclusion constraint cannot express this on a shared facility — the rows
// are not in its index — so the restoration statement carries the guard itself.
func TestClosure_ReopenLeavesOverlappingClosureStanding(t *testing.T) {
	r := newRig(t)

	morning, morningEnd := slot(17, 2*time.Hour) // 17:00-19:00
	evening, eveningEnd := slot(18, 2*time.Hour) // 18:00-20:00

	first := r.close(t, r.gym, morning, morningEnd, "deep clean")
	r.close(t, r.gym, evening, eveningEnd, "equipment delivery")

	r.reopen(t, first.ID)

	// 17:00 was only covered by the withdrawn closure: open again.
	capacity, _, _ := capacityOf(t, r.pg, r.gym, morning)
	require.Equal(t, 30, capacity)

	// 18:00 is still covered by the second closure: it stays shut.
	capacity, _, _ = capacityOf(t, r.pg, r.gym, evening)
	require.Equal(t, 0, capacity, "a slot another closure still covers must not reopen")

	_, err := r.book(r.gym, testutil.StudentID(0), evening, time.Hour)
	require.ErrorIs(t, err, booking.ErrCapacityFull)
}

// TestClosure_ReplayReturnsOriginal: non-negotiable #5 for a mutating endpoint
// whose rows cannot use uq_bookings_user_idem.
//
// A closure has no user_id, and a unique index treats every NULL as distinct, so
// the idempotency index cannot deduplicate closures. The same window submitted
// twice converges on the original row instead — one closure, one audit event, one
// outbox message.
func TestClosure_ReplayReturnsOriginal(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)

	first := r.close(t, r.gym, start, end, "deep clean")
	second := r.close(t, r.gym, start, end, "deep clean")

	require.Equal(t, first.ID, second.ID)
	require.False(t, first.Replayed)
	require.True(t, second.Replayed)

	require.Equal(t, 1, overlapping(t, r.pg, r.gym, start, end), "no second BLOCKED row")
	require.Equal(t, 1, outboxCount(t, r.pg, outbox.TopicClosureCreated), "no second side effect")

	// Over HTTP the replay is a 200 rather than a 201, so the console can tell
	// "closed now" from "was already closed".
	token := r.login(t, "manager01")
	resp := r.do(t, http.MethodPost, "/api/v1/closures", token, map[string]any{
		"facility_id":      r.gym.String(),
		"start":            start.Format(time.RFC3339),
		"duration_minutes": 60,
		"reason":           "deep clean",
	})
	require.Equal(t, http.StatusOK, resp.status, "body: %s", resp.raw)
}

// TestClosure_ListForConsole covers the manager console's read.
func TestClosure_ListForConsole(t *testing.T) {
	r := newRig(t)
	start, end := slot(18, time.Hour)
	later, laterEnd := slot(20, time.Hour)

	court := r.close(t, r.court, start, end, "resurfacing")
	r.close(t, r.gym, later, laterEnd, "deep clean")

	all, err := r.svc.ListClosures(context.Background(), booking.ClosureFilter{})
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "resurfacing", all[0].Reason, "the reason comes from the audit event")
	require.Equal(t, r.manager(), all[0].ActorID)

	byFacility, err := r.svc.ListClosures(context.Background(), booking.ClosureFilter{FacilityID: r.gym})
	require.NoError(t, err)
	require.Len(t, byFacility, 1)
	require.Equal(t, "Gymnasium", byFacility[0].FacilityName)

	byDay, err := r.svc.ListClosures(context.Background(), booking.ClosureFilter{Date: tomorrow()})
	require.NoError(t, err)
	require.Len(t, byDay, 2)

	yesterday := time.Now().In(testutil.IST).AddDate(0, 0, -1).Format("2006-01-02")
	empty, err := r.svc.ListClosures(context.Background(), booking.ClosureFilter{Date: yesterday})
	require.NoError(t, err)
	require.Empty(t, empty)

	// A withdrawn closure is history, not a closure. The console and the
	// availability grid derive from the same rows and cannot disagree.
	r.reopen(t, court.ID)
	remaining, err := r.svc.ListClosures(context.Background(), booking.ClosureFilter{})
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Equal(t, r.gym, remaining[0].FacilityID)
}

// TestClosure_ReopenRejectsANonClosure: this endpoint must not become a second,
// unauthorised way to cancel a student's booking.
func TestClosure_ReopenRejectsANonClosure(t *testing.T) {
	r := newRig(t)
	start, _ := slot(18, time.Hour)

	b := r.mustBook(t, r.court, testutil.StudentID(0), start, time.Hour)

	_, err := r.svc.Reopen(context.Background(), b.ID, r.manager(), "not a closure")
	require.Error(t, err)
	require.ErrorIs(t, err, booking.ErrNotFound)
	require.Equal(t, "CONFIRMED", statusOf(t, r.pg, b.ID))
}

// TestClosure_Validation covers the cheap rejections.
func TestClosure_Validation(t *testing.T) {
	r := newRig(t)
	start, _ := slot(18, time.Hour)

	_, err := r.tryClose(r.court, start, start, "zero length")
	require.ErrorIs(t, err, booking.ErrValidation)

	_, err = r.tryClose(r.court, start, start.Add(-time.Hour), "backwards")
	require.ErrorIs(t, err, booking.ErrValidation)

	_, err = r.tryClose(uuid.New(), start, start.Add(time.Hour), "no such facility")
	require.ErrorIs(t, err, booking.ErrNotFound)
}

// A closure in the PAST is allowed on purpose: a manager recording that the
// court was shut this morning is doing bookkeeping, not booking, and the write
// path's "not in the past" rule is about a student's intent.
func TestClosure_MayCoverAPastWindow(t *testing.T) {
	r := newRig(t)

	day := time.Now().In(testutil.IST).AddDate(0, 0, -1)
	start := time.Date(day.Year(), day.Month(), day.Day(), 18, 0, 0, 0, testutil.IST).UTC()

	c, err := r.tryClose(r.court, start, start.Add(time.Hour), "was shut yesterday")
	require.NoError(t, err)
	require.Equal(t, "BLOCKED", statusOf(t, r.pg, c.ID))
}

// TestClosure_ErrorsAreNotDetectedByString guards the one file rule: only
// store/pgerr.go inspects a SQLSTATE. Nothing here should need it, and this test
// exists so the closure package's errors stay matchable by sentinel.
func TestClosure_ConflictUnwrapsToSlotTaken(t *testing.T) {
	c := &booking.ClosureConflict{FacilityName: "Tennis Court 1"}
	require.True(t, errors.Is(c, booking.ErrSlotTaken))
}
