package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestHTTP_RaceOverHTTP_500Concurrent is the phase gate: the 500-way race run
// through the entire stack — router, middleware, auth, idempotency, shedder,
// handler, service, constraint.
//
// The service-level race already proves the constraint. This proves the STACK
// does not lose that guarantee: no middleware admits a second winner, and
// nothing turns a clean conflict into a 500. A 5xx here would mean the domain
// got it right and the edge threw it away.
func TestHTTP_RaceOverHTTP_500Concurrent(t *testing.T) {
	const n = 500

	a := newAPI(t)
	court := testutil.CourtID()
	start, _ := testutil.Slot18()

	// Distinct users, so the exclusion constraint is what rejects them rather
	// than the idempotency index.
	tokens := make([]string, 0, 10)
	for i := 1; i <= 10; i++ {
		tokens = append(tokens, a.login(t, roll(i)))
	}
	a.pg.Warm(t, 20)
	// Connections before goroutines: see warmHTTP. Without this the 500-way
	// dial burst, not the booking path, decides whether the test runs at all.
	a.warmHTTP(t, n)

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		return a.createBookingResult(ctx, tokens[i%len(tokens)], court, start, 60, uuid.NewString())
	})

	counts := map[int]int{}
	var serverErrors []response
	for _, at := range out.Attempts {
		resp, ok := responseFromAttempt(t, at)
		if !ok {
			continue
		}
		counts[resp.status]++
		if resp.status >= 500 {
			serverErrors = append(serverErrors, resp)
		}
	}

	var dbCount int
	require.NoError(t, a.pg.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM bookings
		 WHERE facility_id = $1 AND status = 'CONFIRMED'`, court).Scan(&dbCount))

	t.Logf("201=%d 409=%d 429=%d other=%d db_count=%d %s",
		counts[201], counts[409], counts[429],
		n-counts[201]-counts[409]-counts[429], dbCount, out.Summarise())

	if len(serverErrors) > 0 {
		t.Fatalf("%d responses were 5xx; the edge must not turn a clean conflict into a "+
			"server error. First: %s", len(serverErrors), serverErrors[0].raw)
	}

	require.Equal(t, 1, counts[201], "exactly one request may create a booking")
	require.Equal(t, 1, dbCount, "the database must hold exactly one confirmed booking")
	require.Equal(t, n, counts[201]+counts[409]+counts[429],
		"every response must be a win, a conflict or a shed")

	// The losers are the majority experience; they must lose correctly.
	require.Positive(t, counts[409]+counts[429])
}

func roll(i int) string {
	return "student" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// TestShed_Returns429WhenQueueFull forces the bound to 1 so the shed path is
// deterministic rather than a matter of timing.
func TestShed_Returns429WhenQueueFull(t *testing.T) {
	const n = 40

	a := newAPI(t, withDepth(1))
	token := a.login(t, "student01")
	start, _ := testutil.Slot18()
	a.pg.Warm(t, 10)
	a.warmHTTP(t, n)

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		return a.createBookingResult(ctx, token, testutil.CourtID(), start, 60, uuid.NewString())
	})

	counts := map[int]int{}
	for _, at := range out.Attempts {
		resp, ok := responseFromAttempt(t, at)
		if ok {
			counts[resp.status]++
		}
	}
	t.Logf("depth=1 201=%d 409=%d 429=%d", counts[201], counts[409], counts[429])

	require.Positive(t, counts[429], "with depth 1 and %d concurrent writes, most must be shed", n)
	require.Equal(t, 1, counts[201])
	require.Zero(t, counts[500])

	// Reads are never shed. Availability is cheap and cacheable, and serving it
	// during a burst is what keeps the screen honest about who won — a student
	// shut out of even looking at the grid learns nothing.
	reads := testutil.Race(t, 40, func(ctx context.Context, i int) (any, error) {
		return a.doResult(ctx, request{method: http.MethodGet, path: "/api/v1/bookings/me", token: token})
	})
	for _, at := range reads.Attempts {
		resp, ok := responseFromAttempt(t, at)
		if !ok {
			continue
		}
		require.Equalf(t, http.StatusOK, resp.status,
			"read %d was shed; only the booking write path is bounded", at.Index)
	}
}

// TestShed_RetryAfterHeaderPresent: a 429 without Retry-After tells the client
// nothing about when to come back, and clients that guess converge on the same
// interval — the herd arriving again on a timer.
func TestShed_RetryAfterHeaderPresent(t *testing.T) {
	const n = 40

	a := newAPI(t, withDepth(1))
	token := a.login(t, "student01")
	start, _ := testutil.Slot18()
	a.warmHTTP(t, n)

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		return a.createBookingResult(ctx, token, testutil.CourtID(), start, 60, uuid.NewString())
	})

	seen := map[string]int{}
	var shed int
	for _, at := range out.Attempts {
		resp, ok := responseFromAttempt(t, at)
		if !ok {
			continue
		}
		if resp.status != http.StatusTooManyRequests {
			continue
		}
		shed++

		retryAfter := resp.headers.Get("Retry-After")
		require.NotEmpty(t, retryAfter, "every 429 must carry Retry-After")
		seen[retryAfter]++

		secs, err := time.ParseDuration(retryAfter + "s")
		require.NoError(t, err, "Retry-After must be whole seconds: %q", retryAfter)
		require.GreaterOrEqual(t, secs, time.Second)
		require.LessOrEqual(t, secs, 3*time.Second)

		require.Equal(t, httpx.CodeShed, resp.errorBody(t).Error)
	}

	require.Positive(t, shed)
	if shed >= 10 {
		require.Greater(t, len(seen), 1,
			"Retry-After must be jittered; every shed client returning at the same "+
				"instant is the same herd on a timer")
	}
}

// responseFromAttempt turns worker errors into test failures in the collecting
// goroutine. It deliberately does not cast until the type assertion succeeds:
// a failed request has no response value, and casting nil was the source of the
// old harness panic that hid the actual connection-refusal error.
func responseFromAttempt(t *testing.T, at testutil.Attempt) (response, bool) {
	t.Helper()
	if at.Err != nil {
		require.NoErrorf(t, at.Err, "HTTP race attempt %d failed", at.Index)
		return response{}, false
	}
	resp, ok := at.Value.(response)
	if !ok {
		require.Truef(t, ok, "HTTP race attempt %d returned %T, want response", at.Index, at.Value)
		return response{}, false
	}
	return resp, true
}
