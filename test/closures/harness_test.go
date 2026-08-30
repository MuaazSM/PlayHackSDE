// Package closures_test covers IMPLEMENTATION.md §10.4: a manager takes a window
// off the board, and the same constraint that stops two students sharing a court
// stops a student booking a closed one.
//
// THE POINT OF THIS SUITE IS THE GYM, not the court. A closure on an exclusive
// facility is nearly free — the BLOCKED row is in no_double_book's index and the
// mechanism already built does the work. On a SHARED facility that row is not in
// the index at all (the predicate is scoped to is_exclusive, §3.2), so it blocks
// precisely nothing until slot_capacity is zeroed alongside it. A suite that only
// tested the court would pass with the gym wide open behind a screen that said
// "closed", which is §18's named risk and the failure this file exists to catch.
//
// Everything runs against a real Postgres. A mock cannot raise 23P01 and cannot
// evaluate `booked < capacity`, which between them are the entire feature.
package closures_test

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
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// rig is one test's worth of wiring: the real booking service, the real read
// path, and the real router over both.
type rig struct {
	pg     *testutil.PG
	cat    *facility.Repo
	svc    *booking.Service
	avail  *facility.Availability
	server *httptest.Server

	court uuid.UUID // exclusive, capacity 1
	gym   uuid.UUID // shared, capacity 30
}

func newRig(t *testing.T) *rig {
	t.Helper()

	pg := testutil.Postgres(t)
	cat := testutil.Catalogue(t, pg)
	court, gym := testutil.CourtID(), testutil.GymID()
	testutil.WarmCatalogue(t, cat, court, gym)

	svc := pg.BookingServiceWith(t, cat)
	avail := facility.NewAvailability(pg.Pool, nil, "Asia/Kolkata", nil)

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

	srv := httptest.NewServer(httpx.NewRouter(httpx.RouterDeps{
		Config:       cfg,
		DB:           pg.DB,
		Bookings:     svc,
		Facilities:   cat,
		Availability: avail,
	}))
	t.Cleanup(srv.Close)

	return &rig{pg: pg, cat: cat, svc: svc, avail: avail, server: srv, court: court, gym: gym}
}

// slot returns [start, end) at TOMORROW's given IST hour, in UTC.
//
// Tomorrow rather than "an hour from now": the courts open 06:00-22:00 IST, this
// suite runs unattended overnight, and a window anchored to the wall clock would
// be outside opening hours for a third of the day. A fixed evening hour on a
// fixed day is inside them whatever time the run starts.
func slot(hour int, d time.Duration) (start, end time.Time) {
	day := time.Now().In(testutil.IST).AddDate(0, 0, 1)
	start = time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, testutil.IST).UTC()
	return start, start.Add(d)
}

// tomorrow is the local date slot() falls on, for the availability queries.
func tomorrow() string {
	return time.Now().In(testutil.IST).AddDate(0, 0, 1).Format("2006-01-02")
}

func (r *rig) manager() uuid.UUID { return testutil.UserIDByRoll("manager01") }

// uuidString is a fresh idempotency key.
func uuidString() string { return uuid.NewString() }

// close blocks a window as the seeded manager.
func (r *rig) close(t *testing.T, facilityID uuid.UUID, start, end time.Time, reason string) *booking.Closure {
	t.Helper()
	c, err := r.tryClose(facilityID, start, end, reason)
	require.NoError(t, err)
	return c
}

func (r *rig) tryClose(facilityID uuid.UUID, start, end time.Time, reason string) (*booking.Closure, error) {
	return r.svc.CreateClosure(context.Background(), booking.ClosureRequest{
		FacilityID: facilityID,
		ActorID:    r.manager(),
		Start:      start,
		End:        end,
		Reason:     reason,
	})
}

func (r *rig) reopen(t *testing.T, id uuid.UUID) *booking.Closure {
	t.Helper()
	c, err := r.svc.Reopen(context.Background(), id, r.manager(), "reopened")
	require.NoError(t, err)
	return c
}

// book is one student's attempt through the real write path.
func (r *rig) book(facilityID, user uuid.UUID, start time.Time, d time.Duration) (*booking.Booking, error) {
	return r.svc.Create(context.Background(), booking.CreateRequest{
		FacilityID: facilityID,
		UserID:     user,
		Start:      start,
		Duration:   d,
		IdemKey:    uuid.NewString(),
	})
}

func (r *rig) mustBook(t *testing.T, facilityID, user uuid.UUID, start time.Time, d time.Duration) *booking.Booking {
	t.Helper()
	b, err := r.book(facilityID, user, start, d)
	require.NoError(t, err)
	return b
}

// ---------------------------------------------------------------------------
// Assertions read the DATABASE, not the return value. The guarantee under test is
// a property of the rows; a test that trusted what the service said would pass
// even if nothing had been written.
// ---------------------------------------------------------------------------

// capacityOf reads a shared slot's counter. found is false when no counter row
// exists yet — which is different from a capacity of zero, and is the difference
// between "never touched" and "closed".
func capacityOf(t *testing.T, pg *testutil.PG, facilityID uuid.UUID, slotStart time.Time) (capacity, booked int, found bool) {
	t.Helper()
	err := pg.Pool.QueryRow(context.Background(),
		`SELECT capacity, booked FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
		facilityID, slotStart.UTC()).Scan(&capacity, &booked)
	if err != nil {
		return 0, 0, false
	}
	return capacity, booked, true
}

func statusOf(t *testing.T, pg *testutil.PG, id uuid.UUID) string {
	t.Helper()
	var s string
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT status::text FROM bookings WHERE id = $1`, id).Scan(&s))
	return s
}

// overlapping counts the rows that hold the window: confirmed bookings, live
// holds and closures. This is the invariant the exclusion constraint exists to
// keep at one for an exclusive facility.
func overlapping(t *testing.T, pg *testutil.PG, facilityID uuid.UUID, start, end time.Time) int {
	t.Helper()
	var n int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM bookings
		  WHERE facility_id = $1
		    AND status IN ('CONFIRMED','HELD','BLOCKED')
		    AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')`,
		facilityID, start.UTC(), end.UTC()).Scan(&n))
	return n
}

func outboxCount(t *testing.T, pg *testutil.PG, topic string) int {
	t.Helper()
	var n int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM outbox WHERE topic = $1`, topic).Scan(&n))
	return n
}

func stateAt(t *testing.T, r *rig, facilityID uuid.UUID, start time.Time) string {
	t.Helper()
	f, err := r.cat.Get(context.Background(), facilityID)
	require.NoError(t, err)

	day, err := r.avail.ForFacility(context.Background(), f, tomorrow())
	require.NoError(t, err)

	for _, s := range day.Slots {
		if s.Start.Equal(start.UTC()) {
			return s.State
		}
	}
	t.Fatalf("no slot starting %s on %s (%d slots)", start.UTC(), tomorrow(), len(day.Slots))
	return ""
}

// ---------------------------------------------------------------------------
// HTTP, for the role check and the shape of the 409. The router is the real one:
// RequireRole is middleware, and a test that called the handler directly would
// skip the very thing it is asserting.

type response struct {
	status int
	raw    []byte
}

func (r response) errorBody(t *testing.T) httpx.ErrorBody {
	t.Helper()
	var e httpx.ErrorBody
	require.NoError(t, json.Unmarshal(r.raw, &e), "body was: %s", r.raw)
	return e
}

func (r *rig) login(t *testing.T, rollNo string) string {
	t.Helper()
	resp := r.do(t, http.MethodPost, "/api/v1/dev/login", "", map[string]any{"roll_no": rollNo})
	require.Equal(t, http.StatusOK, resp.status, "dev login failed: %s", resp.raw)

	var body struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(resp.raw, &body))
	return body.Token
}

func (r *rig) do(t *testing.T, method, path, token string, body any) response {
	t.Helper()

	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		payload = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, r.server.URL+path, payload)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpx.HeaderIdempotencyKey, uuid.NewString())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return response{status: resp.StatusCode, raw: raw}
}
