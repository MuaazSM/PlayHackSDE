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

func TestRateLimit_PerUserEnforced(t *testing.T) {
	rdb := testutil.Redis(t)
	a := newAPI(t, withRedis(rdb), withUserLimit(3))

	token := a.login(t, "student01")

	// Reads are enough to exercise the bucket and keep the write path out of it.
	var statuses []int
	for i := 0; i < 6; i++ {
		resp := a.do(t, request{method: http.MethodGet, path: "/api/v1/bookings/me", token: token})
		statuses = append(statuses, resp.status)
	}

	require.Equal(t, []int{200, 200, 200, 429, 429, 429}, statuses,
		"the fourth request in the window must be refused")

	last := a.do(t, request{method: http.MethodGet, path: "/api/v1/bookings/me", token: token})
	require.Equal(t, httpx.CodeRateLimited, last.errorBody(t).Error,
		"a rate limit is a different 429 from a shed, and the client should be able to tell")
	require.NotEmpty(t, last.headers.Get("Retry-After"))

	// The bucket is per user, so a different student is unaffected.
	other := a.login(t, "student02")
	resp := a.do(t, request{method: http.MethodGet, path: "/api/v1/bookings/me", token: other})
	require.Equal(t, http.StatusOK, resp.status,
		"one student's budget must not be spent by another's")
}

// TestRateLimit_FailsOpenWhenRedisDown is the property that matters most here.
//
// Rate limiting is not a correctness control — the exclusion constraint is — so
// a Redis outage must never be able to take the booking path down. A limiter
// that fails closed converts a cache outage into an outage, which is strictly
// worse than briefly serving unlimited requests.
func TestRateLimit_FailsOpenWhenRedisDown(t *testing.T) {
	// Limits low enough that a working limiter would refuse almost everything.
	a := newAPI(t, withDeadRedis(), withUserLimit(1), withIPLimit(1))

	token := a.login(t, "student01")
	start, _ := testutil.Slot18()

	// The booking still works.
	resp := a.createBooking(t, token, testutil.CourtID(), start, 60, uuid.NewString())
	require.Equal(t, http.StatusCreated, resp.status,
		"a Redis outage must not be able to stop a booking: %s", resp.raw)

	// And keeps working, well past the limit that would otherwise apply.
	for i := 0; i < 10; i++ {
		r := a.do(t, request{method: http.MethodGet, path: "/api/v1/bookings/me", token: token})
		require.Equalf(t, http.StatusOK, r.status,
			"request %d was refused while Redis was down; the limiter must fail open", i)
	}

	// The booking really landed — this is not a 201 over an empty database.
	var n int
	require.NoError(t, a.pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bookings WHERE status = 'CONFIRMED'`).Scan(&n))
	require.Equal(t, 1, n)

	// Readiness is unaffected: Redis degraded is not Redis required.
	ready := a.do(t, request{method: http.MethodGet, path: "/readyz"})
	require.Equal(t, http.StatusOK, ready.status,
		"a Redis outage must not pull every replica out of rotation")

	var body map[string]string
	ready.decode(t, &body)
	require.Equal(t, "ready", body["status"])
	require.Equal(t, "degraded", body["redis"], "but it must be visible that Redis is down")
}

// TestMiddlewareOrder_RateLimitBeforeAuth proves the split around Auth.
//
// If limiting ran only after authentication, an unauthenticated flood would be
// answered with 401 forever and every one of those requests would have cost a
// JWT verification — the flood, not us, would choose how much CPU it burns. With
// the IP bucket in front, an unauthenticated flood is refused before auth runs
// at all, so it returns 429 rather than 401.
func TestMiddlewareOrder_RateLimitBeforeAuth(t *testing.T) {
	rdb := testutil.Redis(t)
	a := newAPI(t, withRedis(rdb), withIPLimit(3))

	// No token on any of these.
	var statuses []int
	for i := 0; i < 6; i++ {
		resp := a.do(t, request{method: http.MethodGet, path: "/api/v1/bookings/me"})
		statuses = append(statuses, resp.status)
	}

	require.Equal(t, []int{401, 401, 401, 429, 429, 429}, statuses,
		"once the IP budget is spent, unauthenticated requests must be refused BEFORE "+
			"auth runs — a 401 here means every flood request is still paying for a "+
			"JWT verification")

	last := a.do(t, request{method: http.MethodGet, path: "/api/v1/bookings/me"})
	require.Equal(t, httpx.CodeRateLimited, last.errorBody(t).Error)

	// Probes sit outside the limiter entirely: a liveness check that can be
	// rate limited will eventually get the container killed during exactly the
	// burst it exists to survive.
	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusOK,
			a.do(t, request{method: http.MethodGet, path: "/healthz"}).status)
	}
}

// TestRateLimit_WindowExpires guards the TTL: only the first hit sets it, so the
// window cannot slide forward on every request and trap a caller indefinitely.
func TestRateLimit_WindowExpires(t *testing.T) {
	rdb := testutil.Redis(t)
	a := newAPI(t, withRedis(rdb), withUserLimit(2))

	token := a.login(t, "student01")

	for i := 0; i < 4; i++ {
		a.do(t, request{method: http.MethodGet, path: "/api/v1/bookings/me", token: token})
	}

	keys, err := rdb.Keys(context.Background(), "rl:user:*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys)

	ttl, err := rdb.TTL(context.Background(), keys[0]).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0), "the bucket must expire, or a caller is trapped forever")
	require.LessOrEqual(t, ttl, time.Minute)
}
