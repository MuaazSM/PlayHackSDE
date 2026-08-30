package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/internal/policy"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// unlimited is a cap high enough to be out of the way, for tests that are
// isolating the OTHER cap.
const unlimited = 1000

func TestPolicy_MaxForwardBookingsEnforced(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := capped(t, pg)
	ctx := context.Background()

	setGlobalPolicy(t, pg, 2, unlimited)

	court, student := testutil.CourtID(), testutil.StudentID(0)

	mustBook(t, ctx, svc, court, student, 1, 6)
	mustBook(t, ctx, svc, court, student, 1, 7)

	// The third is over the cap. Distinct slot, so nothing but the policy could
	// possibly refuse it — the exclusion constraint has no opinion here.
	_, err := book(ctx, svc, court, student, 1, 8)
	require.ErrorIs(t, err, booking.ErrPolicyExceeded)

	var limit *policy.LimitError
	require.ErrorAs(t, err, &limit)
	require.Equal(t, policy.LimitForwardBookings, limit.Limit)

	// The refusal wrote nothing.
	require.Equal(t, 2, forwardCount(t, pg, student))

	// And it is the USER who is capped, not the slot: somebody else takes the
	// same hour without trouble.
	mustBook(t, ctx, svc, court, testutil.StudentID(1), 1, 8)
}

func TestPolicy_MaxWeeklyHoursEnforced(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := capped(t, pg)
	ctx := context.Background()

	setGlobalPolicy(t, pg, unlimited, 3)

	court, student := testutil.CourtID(), testutil.StudentID(0)

	// 1h, 2h, 3h booked — the third lands exactly ON the cap, which is allowed:
	// the rule is "booked hours + this booking <= max_weekly_hours" (§11).
	mustBook(t, ctx, svc, court, student, 1, 6)
	mustBook(t, ctx, svc, court, student, 1, 7)
	mustBook(t, ctx, svc, court, student, 1, 8)

	_, err := book(ctx, svc, court, student, 1, 9)
	require.ErrorIs(t, err, booking.ErrPolicyExceeded)

	var limit *policy.LimitError
	require.ErrorAs(t, err, &limit)
	require.Equal(t, policy.LimitWeeklyHours, limit.Limit)
	require.Equal(t, float64(3), limit.Allowed)
	require.Equal(t, float64(4), limit.Used, "the refusal should quote the total it would have reached")

	require.Equal(t, 3, forwardCount(t, pg, student))
}

// TestPolicy_RollingWindowNotCalendarWeek pins the window to seven days measured
// from now(), not to a calendar week.
//
// The distinction is not pedantry. A calendar week resets at a boundary everyone
// can see, so "ten hours a week" silently becomes "ten hours, and ten more at
// midnight on Sunday" — and the student who books across the boundary beats the
// student who does not. A rolling window has no boundary to aim at.
//
// Two properties are asserted, and between them they kill a
// date_trunc('week', now()) implementation on six days in seven:
//
//   - Bookings five days apart (day+1 and day+6) count against ONE budget. Under
//     a calendar week they only would if today were Monday.
//   - A booking eight days out counts against NOTHING, which pins the width.
//
// The residual is honest and unavoidable: run on a Monday, [now, now+7d) and the
// current calendar week are very nearly the same seven days, so no amount of
// counting can tell them apart without a database clock we can move. Said out
// loud here rather than papered over with a skip.
func TestPolicy_RollingWindowNotCalendarWeek(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := capped(t, pg)
	ctx := context.Background()

	setGlobalPolicy(t, pg, unlimited, 2)

	court, student := testutil.CourtID(), testutil.StudentID(0)

	// Outside the window entirely. It must not consume any of the two hours.
	mustBook(t, ctx, svc, court, student, 8, 6)

	// Both inside, five days apart, straddling whatever boundary a calendar-week
	// implementation would have used.
	mustBook(t, ctx, svc, court, student, 1, 6)
	mustBook(t, ctx, svc, court, student, 6, 6)

	// Two hours are now committed inside the window, so a third is over.
	_, err := book(ctx, svc, court, student, 2, 6)
	require.ErrorIs(t, err, booking.ErrPolicyExceeded)

	var limit *policy.LimitError
	require.ErrorAs(t, err, &limit)
	require.Equal(t, policy.LimitWeeklyHours, limit.Limit)

	// Three bookings survive: the two in the window and the far-future one that
	// was never counted.
	require.Equal(t, 3, forwardCount(t, pg, student))

	// The far side of the window is still open, which is the other half of
	// "rolling": budget is spent per window, not per account.
	mustBook(t, ctx, svc, court, student, 9, 6)
}

func TestPolicy_CancelledBookingsDoNotCount(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := capped(t, pg)
	ctx := context.Background()

	setGlobalPolicy(t, pg, 2, unlimited)

	court, student := testutil.CourtID(), testutil.StudentID(0)

	first := mustBook(t, ctx, svc, court, student, 1, 6)
	mustBook(t, ctx, svc, court, student, 1, 7)

	// At the cap.
	_, err := book(ctx, svc, court, student, 1, 8)
	require.ErrorIs(t, err, booking.ErrPolicyExceeded)

	// Giving one back gives the allowance back. The counters are derived from
	// the bookings table at read time — there is no held-count column that could
	// fail to be decremented, which is non-negotiable #4's argument applied to
	// quotas.
	_, err = svc.Cancel(ctx, first.ID, student, "changed my mind")
	require.NoError(t, err)

	mustBook(t, ctx, svc, court, student, 1, 8)
	require.Equal(t, 2, forwardCount(t, pg, student))

	// And the cancelled hour is not silently still on the books.
	var cancelled int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE user_id = $1 AND status = 'CANCELLED'`,
		student).Scan(&cancelled))
	require.Equal(t, 1, cancelled)
}

// TestPolicy_FacilityOverridesGlobal covers both halves of an override: it wins
// over the global row, and it counts only its own facility's bookings.
//
// The second half is the part worth a test. A facility policy that said "one
// booking" but counted the whole campus would refuse a tennis court because the
// student had booked the gym, which is not what anybody configuring a per-court
// rule means.
func TestPolicy_FacilityOverridesGlobal(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := capped(t, pg)
	ctx := context.Background()

	setGlobalPolicy(t, pg, unlimited, unlimited)

	court, other, student := testutil.CourtID(), testutil.Court2ID(), testutil.StudentID(0)
	setFacilityPolicy(t, pg, court, 1, unlimited)

	mustBook(t, ctx, svc, court, student, 1, 6)

	// Second on the overridden court: refused by the override, not the global.
	_, err := book(ctx, svc, court, student, 1, 7)
	require.ErrorIs(t, err, booking.ErrPolicyExceeded)

	// The other court is governed by the global row and is unbothered — twice,
	// which the override's cap of 1 would have refused had it applied.
	mustBook(t, ctx, svc, other, student, 1, 6)
	mustBook(t, ctx, svc, other, student, 1, 7)

	// Nor do those two count against the overridden court: still exactly one
	// booking's worth of allowance used there, and still refused.
	_, err = book(ctx, svc, court, student, 1, 8)
	require.ErrorIs(t, err, booking.ErrPolicyExceeded)

	require.Equal(t, 3, forwardCount(t, pg, student))
}

// TestPolicy_NoPolicyRowMeansUnlimited is the fail-open case.
//
// A quota system that refused every booking because nobody had configured it
// would be a worse outage than the hoarding it prevents, and "unconfigured" is
// the state a fresh database is in.
func TestPolicy_NoPolicyRowMeansUnlimited(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := capped(t, pg)
	ctx := context.Background()

	clearPolicies(t, pg)

	court, student := testutil.CourtID(), testutil.StudentID(0)

	// Well past the seeded caps of 3 forward bookings and 10 weekly hours.
	for hour := 6; hour <= 17; hour++ {
		mustBook(t, ctx, svc, court, student, 1, hour)
	}
	require.Equal(t, 12, forwardCount(t, pg, student))
}

// TestPolicy_422PayloadNamesLimitAndReset checks the wire contract from §11:
//
//	{"error":"POLICY_LIMIT","limit":"max_weekly_hours","resets_at":"…"}
//
// Both members matter to the student on the other end. Without `limit` they
// cannot tell whether to cancel something or pick another sport; without
// `resets_at` they are told to wait without being told how long.
func TestPolicy_422PayloadNamesLimitAndReset(t *testing.T) {
	t.Run("forward bookings", func(t *testing.T) {
		pg := testutil.Postgres(t)
		a := newAPI(t, pg)
		setGlobalPolicy(t, pg, 1, unlimited)

		token := a.login(t, "student01")
		court := testutil.CourtID()

		status, _ := a.bookHTTP(t, token, court, 1, 6)
		require.Equal(t, http.StatusCreated, status)

		status, raw := a.bookHTTP(t, token, court, 1, 7)
		require.Equal(t, http.StatusUnprocessableEntity, status, "body: %s", raw)

		body := decodeError(t, raw)
		require.Equal(t, httpx.CodePolicyLimit, body.Error)
		require.Equal(t, policy.LimitForwardBookings, body.Limit)
		require.NotEmpty(t, body.Message, "the envelope always carries a sentence")

		// The forward allowance frees up when the earliest held booking starts.
		firstStart, _ := slot(1, 6, time.Hour)
		require.NotNil(t, body.ResetsAt)
		require.True(t, body.ResetsAt.Equal(firstStart),
			"resets_at %s should be the earliest booking's start %s", body.ResetsAt, firstStart)
	})

	t.Run("weekly hours", func(t *testing.T) {
		pg := testutil.Postgres(t)
		a := newAPI(t, pg)
		setGlobalPolicy(t, pg, unlimited, 1)

		token := a.login(t, "student01")
		court := testutil.CourtID()

		status, _ := a.bookHTTP(t, token, court, 1, 6)
		require.Equal(t, http.StatusCreated, status)

		status, raw := a.bookHTTP(t, token, court, 1, 7)
		require.Equal(t, http.StatusUnprocessableEntity, status, "body: %s", raw)

		body := decodeError(t, raw)
		require.Equal(t, httpx.CodePolicyLimit, body.Error)
		require.Equal(t, policy.LimitWeeklyHours, body.Limit)

		// Hours come back when the earliest counted booking ENDS, which is a
		// different instant from the forward case above — the two limits do not
		// share a reset and must not pretend to.
		_, firstEnd := slot(1, 6, time.Hour)
		require.NotNil(t, body.ResetsAt)
		require.True(t, body.ResetsAt.Equal(firstEnd),
			"resets_at %s should be the earliest counted booking's end %s", body.ResetsAt, firstEnd)
	})
}

// TestPolicy_AdvisoryUnderRace DOCUMENTS a behaviour. It is not a bug report and
// the assertion is deliberately loose.
//
// IMPLEMENTATION.md §4.7: the fair-use check is a read-then-write inside the
// transaction with nothing serialising the pair, so under perfectly simultaneous
// requests a student can land one — or here, several — bookings over their cap.
// Five goroutines against five DIFFERENT facilities is the worst case on
// purpose: different facilities take different advisory locks, so nothing at all
// orders the five policy reads and every one of them can observe an empty
// account.
//
// This is accepted, and the distinction is the whole point of the file it is
// documented in. Slot uniqueness is enforced by a Postgres exclusion constraint
// and is ABSOLUTE — no arrangement of concurrency produces two confirmed
// bookings for one court and hour. A fair-use cap is a policy knob whose worst
// failure is one student holding five courts instead of three. Buying the second
// a hard guarantee (an exclusion constraint, a lock, SELECT FOR UPDATE) would
// cost the clarity of the first, which is the one thing this project is judged
// on. If this test ever starts asserting exactly 3, something has quietly
// changed that answer.
func TestPolicy_AdvisoryUnderRace(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := capped(t, pg)

	const cap0 = 3
	setGlobalPolicy(t, pg, cap0, unlimited)

	student := testutil.StudentID(0)
	facilities := testutil.ExclusiveFacilityIDs()[:5]
	start, _ := slot(1, 6, time.Hour)

	// Warm the catalogue and the pool so the goroutines are actually racing the
	// policy check rather than a cache miss and a connection dial.
	testutil.WarmCatalogue(t, testutil.Catalogue(t, pg), facilities...)
	pg.Warm(t, len(facilities))

	out := testutil.Race(t, len(facilities), func(ctx context.Context, i int) (any, error) {
		return svc.Create(ctx, booking.CreateRequest{
			FacilityID: facilities[i],
			UserID:     student,
			Start:      start,
			Duration:   time.Hour,
			IdemKey:    uuid.NewString(),
		})
	})

	var confirmed, refused int
	for _, a := range out.Attempts {
		switch {
		case a.Err == nil:
			confirmed++
		case isPolicy(a.Err):
			refused++
		default:
			t.Fatalf("unexpected error from a concurrent booking: %v", a.Err)
		}
	}

	// Every outcome is accounted for, and the cap held to within the one thing
	// an advisory check can guarantee: it never refuses somebody who was under
	// the cap, and it never lets in more than the herd size.
	require.Equal(t, len(facilities), confirmed+refused)
	require.GreaterOrEqual(t, confirmed, cap0,
		"an advisory check must never refuse a student who was under their cap")
	require.LessOrEqual(t, confirmed, len(facilities))

	// The database agrees with what the callers were told. Whatever the split
	// was, it is not a phantom.
	require.Equal(t, confirmed, forwardCount(t, pg, student))

	t.Logf("advisory fair-use cap: cap=%d concurrent=%d confirmed=%d refused=%d (§4.7)",
		cap0, len(facilities), confirmed, refused)

	// Advisory is not the same as absent, and this stops the assertion above
	// from passing vacuously if the caps were ever switched off: once the herd
	// has cleared, a sequential request from the same student — now plainly over
	// the cap — is refused.
	_, err := book(context.Background(), svc, testutil.ExclusiveFacilityIDs()[5], student, 1, 7)
	require.ErrorIs(t, err, booking.ErrPolicyExceeded,
		"the cap is advisory under simultaneity, not absent")
}

func isPolicy(err error) bool { return errors.Is(err, booking.ErrPolicyExceeded) }

// decodeError unmarshals the one error envelope (§10.3).
func decodeError(t *testing.T, raw []byte) httpx.ErrorBody {
	t.Helper()
	var body httpx.ErrorBody
	require.NoError(t, json.Unmarshal(raw, &body), "body was: %s", raw)
	return body
}
