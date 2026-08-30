// Package policy_test exercises the fair-use caps and the priority tiers
// against a real Postgres.
//
// Nothing here is mocked, for the usual reason: the caps are evaluated by SQL
// against real booking rows inside a real transaction, and the one property this
// package has to be honest about — that the check is ADVISORY under simultaneity
// (IMPLEMENTATION.md §4.7) — only exists because two real transactions can
// overlap. A fake would show whatever it was told to.
package policy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/internal/policy"
	"github.com/iitg-playhack/sportsbook/internal/waitlist"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// capped wires the write path WITH fair-use caps.
//
// booking.NewService leaves them off, which is what every other suite in this
// repository gets; a caps test has to opt in explicitly, exactly like cmd/api
// does.
func capped(t *testing.T, pg *testutil.PG) *booking.Service {
	t.Helper()
	return pg.BookingService(t).WithPolicy(policy.Check)
}

// cappedWithPromotion adds the waitlist to the cancel path, for the priority
// tests: a tier only becomes observable when somebody is actually promoted.
func cappedWithPromotion(t *testing.T, pg *testutil.PG) (*booking.Service, *waitlist.Service) {
	t.Helper()
	wl := waitlist.NewService(pg.DB, testutil.Catalogue(t, pg), time.Minute, nil)
	return capped(t, pg).WithPromotion(wl), wl
}

// ---------------------------------------------------------------------------
// Policy rows
// ---------------------------------------------------------------------------

// setGlobalPolicy rewrites the single facility_id IS NULL row the seed creates.
func setGlobalPolicy(t *testing.T, pg *testutil.PG, forward, weeklyHours int) {
	t.Helper()
	tag, err := pg.Pool.Exec(context.Background(),
		`UPDATE policies SET max_forward_bookings = $1, max_weekly_hours = $2
		  WHERE facility_id IS NULL`, forward, weeklyHours)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "seed should leave exactly one global policy row")
}

// setFacilityPolicy adds or replaces a facility override.
func setFacilityPolicy(t *testing.T, pg *testutil.PG, facilityID uuid.UUID, forward, weeklyHours int) {
	t.Helper()
	_, err := pg.Pool.Exec(context.Background(),
		`INSERT INTO policies (facility_id, max_forward_bookings, max_weekly_hours)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (facility_id) DO UPDATE
		    SET max_forward_bookings = EXCLUDED.max_forward_bookings,
		        max_weekly_hours     = EXCLUDED.max_weekly_hours`,
		facilityID, forward, weeklyHours)
	require.NoError(t, err)
}

// clearPolicies removes every policy row, which is the "nothing configured"
// state.
func clearPolicies(t *testing.T, pg *testutil.PG) {
	t.Helper()
	_, err := pg.Pool.Exec(context.Background(), `DELETE FROM policies`)
	require.NoError(t, err)
}

// setTier moves a user between priority tiers (migration 0010).
func setTier(t *testing.T, pg *testutil.PG, userID uuid.UUID, tier policy.Tier) {
	t.Helper()
	tag, err := pg.Pool.Exec(context.Background(),
		`UPDATE users SET tier = $2::priority_tier WHERE id = $1`, userID, string(tier))
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

// addNoShow writes a NO_SHOW booking daysAgo in the past.
//
// Inserted directly rather than driven through the sweeper: this is a fixture
// for the priority calculation, and §7's sweep has its own suite. NO_SHOW is
// outside no_double_book's predicate, so these rows collide with nothing.
func addNoShow(t *testing.T, pg *testutil.PG, userID, facilityID uuid.UUID, daysAgo int) {
	t.Helper()
	start := time.Now().UTC().AddDate(0, 0, -daysAgo)
	_, err := pg.Pool.Exec(context.Background(),
		`INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status)
		 VALUES ($1, $2, true, tstzrange($3::timestamptz, $4::timestamptz, '[)'), 'NO_SHOW')`,
		facilityID, userID, start, start.Add(time.Hour))
	require.NoError(t, err)
}

// forwardCount is how many upcoming bookings a user holds, straight from the
// table. The assertion of record: the caps exist to bound this number.
func forwardCount(t *testing.T, pg *testutil.PG, userID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bookings
		  WHERE user_id = $1 AND status IN ('CONFIRMED','HELD') AND lower(during) >= now()`,
		userID).Scan(&n))
	return n
}

// ---------------------------------------------------------------------------
// Slots
// ---------------------------------------------------------------------------

// slot returns [start, end) at the given IST hour, dayOffset days from today.
//
// Deliberately not testutil.Slot18, which pins to TODAY: every booking here has
// to be in the future for the write path to accept it, and several of these
// tests place bookings a week out to probe the rolling window.
func slot(dayOffset, hour int, d time.Duration) (start, end time.Time) {
	now := time.Now().In(testutil.IST)
	start = time.Date(now.Year(), now.Month(), now.Day()+dayOffset, hour, 0, 0, 0, testutil.IST).UTC()
	return start, start.Add(d)
}

// book puts one hour on the court at the given day/hour through the real write
// path, and returns whatever the service said.
func book(ctx context.Context, svc *booking.Service, facilityID, userID uuid.UUID, dayOffset, hour int) (*booking.Booking, error) {
	start, _ := slot(dayOffset, hour, time.Hour)
	return svc.Create(ctx, booking.CreateRequest{
		FacilityID: facilityID,
		UserID:     userID,
		Start:      start,
		Duration:   time.Hour,
		IdemKey:    uuid.NewString(),
	})
}

// mustBook fails the test if the booking was refused.
func mustBook(t *testing.T, ctx context.Context, svc *booking.Service, facilityID, userID uuid.UUID, dayOffset, hour int) *booking.Booking {
	t.Helper()
	b, err := book(ctx, svc, facilityID, userID, dayOffset, hour)
	require.NoError(t, err, "day+%d %02d:00 should have been allowed", dayOffset, hour)
	return b
}

// ---------------------------------------------------------------------------
// HTTP — for the 422 envelope
// ---------------------------------------------------------------------------

type api struct {
	server *httptest.Server
	pg     *testutil.PG
}

// newAPI starts the real router over a policy-enabled booking service.
//
// httptest rather than calling the handler: the 422 body is rendered by
// httpx.Error, which is reached through the middleware chain, and a test that
// invokes the handler directly would be asserting on a body nobody serves.
func newAPI(t *testing.T, pg *testutil.PG) *api {
	t.Helper()

	cfg := &config.Config{
		DBURL:               pg.DSN,
		DBMaxConns:          20,
		AuthMode:            config.AuthModeDev,
		JWTSecret:           "test-secret",
		WriteQueueDepth:     config.DefaultWriteQueueDepth,
		WriteTimeout:        5 * time.Second,
		TZDisplay:           "Asia/Kolkata",
		RateLimitIPPerMin:   100000,
		RateLimitUserPerMin: 100000,
	}

	loc, err := time.LoadLocation(cfg.TZDisplay)
	require.NoError(t, err)

	facilities := facility.NewRepo(pg.Pool)
	svc := booking.NewService(pg.DB, facilities, loc).WithPolicy(policy.Check)

	srv := httptest.NewServer(httpx.NewRouter(httpx.RouterDeps{
		Config:       cfg,
		DB:           pg.DB,
		Bookings:     svc,
		Facilities:   facilities,
		Availability: facility.NewAvailability(pg.DB.Replica, nil, cfg.TZDisplay, nil),
	}))
	t.Cleanup(srv.Close)

	return &api{server: srv, pg: pg}
}

func (a *api) login(t *testing.T, rollNo string) string {
	t.Helper()
	status, raw := a.do(t, http.MethodPost, "/api/v1/dev/login", "", map[string]any{"roll_no": rollNo})
	require.Equal(t, http.StatusOK, status, "dev login failed: %s", raw)

	var body struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	return body.Token
}

// bookHTTP posts one booking and returns the status and raw body.
func (a *api) bookHTTP(t *testing.T, token string, facilityID uuid.UUID, dayOffset, hour int) (int, []byte) {
	t.Helper()
	start, _ := slot(dayOffset, hour, time.Hour)
	return a.do(t, http.MethodPost, "/api/v1/bookings", token, map[string]any{
		"facility_id":      facilityID.String(),
		"start":            start.Format(time.RFC3339),
		"duration_minutes": 60,
	})
}

func (a *api) do(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()

	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, a.server.URL+path, r)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpx.HeaderIdempotencyKey, uuid.NewString())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := a.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}
