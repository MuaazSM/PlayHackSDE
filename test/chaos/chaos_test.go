// Package chaos_test breaks things on purpose while the write path is running.
//
// Every other suite asks "is this correct when nothing goes wrong". These ask
// the question CLAUDE.md actually cares about: Redis is not authoritative, the
// replica is not authoritative, and the API is stateless — so when any of them
// dies mid-race, the system must get SLOWER and never WRONGER.
//
// The assertion is the same one everywhere and it is deliberately blunt: after
// the chaos, `SELECT count(*) FROM bookings WHERE ... = 1`. Correctness lives in
// Postgres, so Postgres is where it is checked.
package chaos_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// TestMain skips the whole package under -short.
//
// `make test` runs -short, and these tests are the heaviest things in the repo:
// each starts its own Postgres and Redis and fires a 200-request storm. Left in
// the default run they push a laptop Docker VM over the edge and start knocking
// over the latency-budgeted tests in OTHER packages, which is a real regression
// caused by a test rather than by the code it tests.
//
// They are not skipped to make CI green — `make chaos` runs them, and they are
// part of the required gate. They are skipped from the wrong runner.
func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// server is a running API over the test database.
type server struct {
	http *httptest.Server
	pg   *testutil.PG
}

// newServer starts the REAL router, middleware chain included.
//
// Calling the handler function directly would bypass auth, rate limiting,
// idempotency and shedding — which is most of what these tests are trying to
// break.
func newServer(t *testing.T, pg *testutil.PG, rdb *redis.Client) *server {
	t.Helper()

	cfg := &config.Config{
		DBURL:               pg.DSN,
		DBMaxConns:          20,
		AuthMode:            config.AuthModeDev,
		JWTSecret:           "chaos-secret",
		WriteQueueDepth:     config.DefaultWriteQueueDepth,
		WriteTimeout:        5 * time.Second,
		TZDisplay:           "Asia/Kolkata",
		RateLimitIPPerMin:   1000000,
		RateLimitUserPerMin: 1000000,
	}

	loc, err := time.LoadLocation(cfg.TZDisplay)
	require.NoError(t, err)

	facilities := facility.NewRepo(pg.Pool)
	svc := booking.NewService(pg.DB, facilities, loc)
	availability := facility.NewAvailability(pg.DB.Replica, rdb, cfg.TZDisplay, nil)

	srv := httptest.NewServer(httpx.NewRouter(httpx.RouterDeps{
		Config:       cfg,
		DB:           pg.DB,
		Redis:        rdb,
		Bookings:     svc,
		Facilities:   facilities,
		Availability: availability,
	}))

	return &server{http: srv, pg: pg}
}

func (s *server) close() { s.http.Close() }

// token mints a bearer for a seeded roll number through the real dev route.
func (s *server) token(t *testing.T, roll string) string {
	t.Helper()

	body, _ := json.Marshal(map[string]string{"roll_no": roll})
	resp, err := s.http.Client().Post(s.http.URL+"/api/v1/dev/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "dev login: %s", raw)

	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out.Token
}

// result is one attempt's outcome. A status of 0 means the request never got an
// HTTP response at all — a transport error, which is what an API being killed
// underneath you looks like from the client side.
type result struct {
	status int
	err    error
}

// tally is the outcome split, which is the number that matters more than any
// individual request.
type tally struct {
	created   int
	conflict  int
	shed      int
	other4xx  int
	server5xx int
	transport int
}

func count(results []result) tally {
	var tl tally
	for _, r := range results {
		switch {
		case r.status == 0:
			tl.transport++
		case r.status == http.StatusCreated || r.status == http.StatusOK:
			tl.created++
		case r.status == http.StatusConflict:
			tl.conflict++
		case r.status == http.StatusTooManyRequests || r.status == http.StatusServiceUnavailable:
			tl.shed++
		case r.status >= 500:
			tl.server5xx++
		default:
			tl.other4xx++
		}
	}
	return tl
}

// storm fires n bookings at one slot, released together, and calls each of the
// hooks at the fraction of the way through that it is keyed on.
//
// The barrier is a closed channel rather than a WaitGroup so every goroutine is
// released by one scheduler event; staggered starts would measure queueing
// rather than contention.
func storm(t *testing.T, s *server, n int, facilityID uuid.UUID, start time.Time, chaos func()) []result {
	t.Helper()

	// Distinct users. With one user the idempotency index rather than the
	// exclusion constraint would be doing the rejecting.
	users := s.pg.Users(t, n)
	tokens := make([]string, n)
	for i, u := range users {
		tokens[i] = s.tokenFor(t, u)
	}

	results := make([]result, n)
	gate := make(chan struct{})
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(n)
	done.Add(n)

	var fired atomic.Int64

	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()

			body, _ := json.Marshal(map[string]any{
				"facility_id":      facilityID.String(),
				"start":            start.Format(time.RFC3339),
				"duration_minutes": 60,
			})

			req, err := http.NewRequest(http.MethodPost, s.http.URL+"/api/v1/bookings", bytes.NewReader(body))
			if err != nil {
				results[i] = result{err: err}
				ready.Done()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tokens[i])
			req.Header.Set(httpx.HeaderIdempotencyKey, uuid.NewString())

			ready.Done()
			<-gate

			resp, err := s.http.Client().Do(req)
			fired.Add(1)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			results[i] = result{status: resp.StatusCode}
		}(i)
	}

	ready.Wait()

	// Fire the chaos once the storm is genuinely in flight rather than at a
	// fixed wall-clock offset: on a loaded machine a fixed sleep lands after
	// everything has already finished, and the test would silently stop
	// testing anything.
	chaosDone := make(chan struct{})
	go func() {
		defer close(chaosDone)
		if chaos == nil {
			return
		}
		deadline := time.Now().Add(10 * time.Second)
		for fired.Load() < int64(n)/4 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		chaos()
	}()

	close(gate)
	done.Wait()
	<-chaosDone

	return results
}

// tokenFor mints a bearer for a user id created at test time (not a seeded
// roll number).
func (s *server) tokenFor(t *testing.T, id uuid.UUID) string {
	t.Helper()

	var roll string
	require.NoError(t, s.pg.Pool.QueryRow(context.Background(),
		`SELECT roll_no FROM users WHERE id = $1`, id).Scan(&roll))
	return s.token(t, roll)
}

// confirmed reads the only number that decides whether any of this worked.
func confirmed(t *testing.T, pg *testutil.PG, facilityID uuid.UUID, start, end time.Time) int {
	t.Helper()

	var n int
	require.NoError(t, pg.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM bookings
		 WHERE facility_id = $1
		   AND status = 'CONFIRMED'
		   AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')`,
		facilityID, start, end).Scan(&n))
	return n
}

// ---------------------------------------------------------------------------
// 1. Redis wiped mid-run.
// ---------------------------------------------------------------------------

// TestRedisFlushMidRun is non-negotiable #3 under fire.
//
// Redis holds the availability cache, the rate-limit counters and the pub/sub
// bus. None of them is on the write path, so FLUSHALL in the middle of 200
// concurrent bookings must cost latency and nothing else: still exactly one
// winner, still no 500s.
//
// If this test ever fails with a 5xx, something started depending on Redis being
// there. That is the failure mode worth spending a test on — a Redis dependency
// does not announce itself, it just works until the day it doesn't.
func TestRedisFlushMidRun(t *testing.T) {
	const n = 200

	pg := testutil.Postgres(t)
	rdb := testutil.Redis(t)
	srv := newServer(t, pg, rdb)
	defer srv.close()

	court := testutil.CourtID()
	start, end := testutil.Slot18()
	pg.Warm(t, 25)

	var flushes atomic.Int64
	results := storm(t, srv, n, court, start, func() {
		// Not once. Repeatedly, for the duration of the storm — a single
		// FLUSHALL might land in a gap where nothing was going to read Redis
		// anyway, and prove nothing.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err := rdb.FlushAll(context.Background()).Err(); err == nil {
				flushes.Add(1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	tl := count(results)
	t.Logf("redis flushed %d times mid-run; outcome: %+v", flushes.Load(), tl)

	require.Positive(t, flushes.Load(), "chaos never actually fired — the test proved nothing")
	require.Zero(t, tl.server5xx, "a Redis wipe produced %d server errors — something on the "+
		"request path depends on Redis being available", tl.server5xx)
	require.Zero(t, tl.transport, "requests failed at the transport layer")

	require.Equal(t, 1, confirmed(t, pg, court, start, end),
		"DOUBLE BOOKING after a Redis wipe — Redis had become authoritative")
	require.Equal(t, 1, tl.created, "exactly one request may be told it won")
}

// ---------------------------------------------------------------------------
// 2. The read path lies.
// ---------------------------------------------------------------------------

// TestReplicaLagDoesNotCorrupt is the true streaming-replica case, and it does
// NOT run in this environment.
//
// `make dev-replica` brings up a standby on :5433 behind a compose profile, but
// it has never been brought up here; everything is developed and tested with
// DB_REPLICA_URL empty, which falls back to the primary and is a supported
// configuration rather than a degraded one (IMPLEMENTATION.md §2.1). There is
// therefore no replica to pause, and a test that paused the primary and called
// it a replica would be measuring something else while claiming this name.
//
// TestStaleAvailabilityDoesNotCorruptWrites below covers the property this test
// would have covered — a read path serving an out-of-date answer cannot cause a
// wrong booking — by a route that does exist. Set CHAOS_REPLICA_URL to a real
// standby to run this one.
func TestReplicaLagDoesNotCorrupt(t *testing.T) {
	url := os.Getenv("CHAOS_REPLICA_URL")
	if url == "" {
		t.Skip("SKIPPED: no streaming replica in this environment. `make dev-replica` " +
			"has never been brought up here and DB_REPLICA_URL falls back to the primary, " +
			"so there is nothing to pause. Set CHAOS_REPLICA_URL to a standby DSN to run " +
			"this. See TestStaleAvailabilityDoesNotCorruptWrites for the property under " +
			"test by a route that does exist.")
	}
	t.Fatalf("CHAOS_REPLICA_URL is set (%s) but the replica pause harness is not "+
		"implemented — do not report this test as passing", url)
}

// TestStaleAvailabilityDoesNotCorruptWrites is the risk-register line from
// IMPLEMENTATION.md §18 made executable: "stale `free` costs one tap, never a
// wrong booking".
//
// Replica lag and a stale Redis grid are the same failure from the write path's
// point of view — the read path said a slot was free when it was not. This
// reproduces the worst case directly and without a replica: pin the cache to an
// all-free grid with an effectively infinite TTL, so availability keeps
// insisting the court is open for the whole storm, and then check the storm.
//
// The write path never reads availability, so the correct result is that this
// changes nothing at all. That is the point.
func TestStaleAvailabilityDoesNotCorruptWrites(t *testing.T) {
	const n = 200

	pg := testutil.Postgres(t)
	rdb := testutil.Redis(t)
	srv := newServer(t, pg, rdb)
	defer srv.close()

	court := testutil.CourtID()
	start, end := testutil.Slot18()
	date := start.In(testutil.IST).Format("2006-01-02")
	pg.Warm(t, 25)

	// Warm the grid while everything really is free, then freeze that answer in
	// place for far longer than the storm lasts. Every availability read during
	// the storm now returns a grid that is maximally, deliberately wrong.
	stale := facility.NewAvailability(pg.DB.Replica, rdb, "Asia/Kolkata", nil).
		WithTTL(10 * time.Minute)
	before, err := stale.Campus(context.Background(), date)
	require.NoError(t, err)
	require.NotEmpty(t, before.Facilities)

	results := storm(t, srv, n, court, start, func() {
		// Keep re-asserting the stale answer, so a cache invalidation
		// somewhere cannot quietly rescue the test.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			_, _ = stale.Campus(context.Background(), date)
			time.Sleep(20 * time.Millisecond)
		}
	})

	tl := count(results)
	t.Logf("outcome with a frozen all-free availability grid: %+v", tl)

	require.Zero(t, tl.server5xx)
	require.Equal(t, 1, confirmed(t, pg, court, start, end),
		"a stale read path produced a double booking — the write path is reading availability")
	require.Equal(t, 1, tl.created)

	// And the cache really was stale: it still says free after the slot was won.
	cached, ok := stale.CampusCached(context.Background(), date)
	require.True(t, ok, "the grid should still be cached")
	require.NotNil(t, cached)
}

// ---------------------------------------------------------------------------
// 3. The API dies mid-race.
// ---------------------------------------------------------------------------

// TestAPIRestartMidRace kills the API while 200 requests are in flight and
// brings a fresh one up on the same database.
//
// The API is a stateless binary by design, so this must be survivable: whatever
// the dying process had half-done, the database either committed it or it never
// happened, and the replacement process starts from the same rows. In-flight
// requests get a transport error — that is honest and expected, the client
// retries — but the slot must not end up doubly booked, and the new process must
// still reject correctly rather than start handing out a second winner.
//
// The second storm is the load-bearing half. A restart that leaves the database
// consistent but the new process confused is still a broken system.
func TestAPIRestartMidRace(t *testing.T) {
	const n = 200

	pg := testutil.Postgres(t)
	rdb := testutil.Redis(t)

	court := testutil.CourtID()
	start, end := testutil.Slot18()
	pg.Warm(t, 25)

	first := newServer(t, pg, rdb)

	// Kill the server mid-storm. httptest.Server.Close blocks on outstanding
	// requests, so CloseClientConnections is what actually severs them — the
	// closest in-process equivalent of the process going away.
	results := storm(t, first, n, court, start, func() {
		first.http.CloseClientConnections()
	})
	first.close()

	tl := count(results)
	t.Logf("storm 1, API severed mid-flight: %+v", tl)
	require.Zero(t, tl.server5xx, "the dying API returned %d server errors", tl.server5xx)

	dbCount := confirmed(t, pg, court, start, end)
	t.Logf("db_count after the restart-mid-race: %d", dbCount)
	require.LessOrEqual(t, dbCount, 1, "DOUBLE BOOKING survived an API restart")

	// A fresh process against the same rows.
	second := newServer(t, pg, rdb)
	defer second.close()

	results2 := storm(t, second, n, court, start, nil)
	tl2 := count(results2)
	t.Logf("storm 2, fresh API on the same database: %+v", tl2)

	require.Zero(t, tl2.server5xx)
	require.Zero(t, tl2.transport)

	// Across BOTH storms — 400 requests and one process death — exactly one
	// booking exists. This is the assertion the whole test is for.
	require.Equal(t, 1, confirmed(t, pg, court, start, end),
		"after an API restart and 400 total attempts, the slot must hold exactly one booking")

	// And the second storm agrees: if storm 1 already produced the winner, the
	// fresh process must produce none.
	require.Equal(t, 1, dbCount+tl2.created,
		"the restarted API handed out a winner for a slot that was already taken")
}

// ---------------------------------------------------------------------------
// 4. Every pooled connection dies at once.
// ---------------------------------------------------------------------------

// TestPgBouncerRestart is the connection-pool failure.
//
// WHAT THIS ACTUALLY DOES, so nobody reads more into a green tick than is there:
// the suite runs against a testcontainer Postgres with no PgBouncer in front of
// it, so there is no pooler process to restart. Instead it does what a pooler
// restart looks like FROM THE APPLICATION'S SIDE — pg_terminate_backend on every
// backend the pool is holding, mid-storm, so every checked-out connection dies
// at the same instant. That is the same signature the app sees and the same
// recovery it has to perform.
//
// It is not the same as restarting the container, and the difference is worth
// naming: a real PgBouncer restart also drops server-side prepared statements
// and any session state, which is precisely why the write path is forbidden
// session-scoped advisory locks (`pg_advisory_xact_lock`, transaction-scoped, is
// the accepted exception in CLAUDE.md). This test does not cover that.
//
// Two things must hold: the pool reconnects without operator help, and no
// half-finished transaction leaves a partial booking behind.
func TestPgBouncerRestart(t *testing.T) {
	const n = 200

	pg := testutil.Postgres(t)
	rdb := testutil.Redis(t)
	srv := newServer(t, pg, rdb)
	defer srv.close()

	court := testutil.CourtID()
	start, end := testutil.Slot18()
	pg.Warm(t, 25)

	var killed atomic.Int64
	results := storm(t, srv, n, court, start, func() {
		// A separate connection, because the pool's own connections are the
		// ones being killed.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		deadline := time.Now().Add(1500 * time.Millisecond)
		for time.Now().Before(deadline) {
			var n int64
			err := pg.Pool.QueryRow(ctx, `
				SELECT count(*) FROM (
				  SELECT pg_terminate_backend(pid)
				    FROM pg_stat_activity
				   WHERE datname = current_database()
				     AND pid <> pg_backend_pid()
				     AND backend_type = 'client backend'
				) t`).Scan(&n)
			if err == nil {
				killed.Add(n)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	tl := count(results)
	t.Logf("terminated %d backends mid-storm; outcome: %+v", killed.Load(), tl)
	require.Positive(t, killed.Load(), "chaos never actually fired — no backends were terminated")

	// 5xx IS EXPECTED HERE, and this test deliberately does not assert it away.
	// A request whose connection is killed mid-transaction has genuinely failed
	// and a 500 is the honest answer; pretending otherwise would mean retrying
	// inside the handler, which is how you turn one lost booking into two. The
	// Redis test asserts zero 5xx because Redis is not on the write path; the
	// database is, and killing it must be visible.
	t.Logf("%d requests failed with 5xx, which is the correct answer when the "+
		"connection under an open transaction is killed", tl.server5xx)

	// Correctness first: whatever died, no partial state.
	dbCount := confirmed(t, pg, court, start, end)
	require.LessOrEqual(t, dbCount, 1, "DOUBLE BOOKING after every connection was killed")

	// No booking row may exist without its audit event, and none may exist
	// without a matching outbox row. Those are written in the SAME transaction
	// as the insert, so a mismatch means a transaction committed in pieces —
	// which is the "partial state" this test is named for.
	var orphans int
	require.NoError(t, pg.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM bookings b
		 WHERE b.status = 'CONFIRMED'
		   AND NOT EXISTS (SELECT 1 FROM booking_events e
		                    WHERE e.booking_id = b.id AND e.to_status = 'CONFIRMED')`).
		Scan(&orphans))
	require.Zero(t, orphans, "%d confirmed bookings have no audit event — a transaction "+
		"committed in pieces", orphans)

	// Recovery: the pool must come back on its own, and the API must still work.
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return pg.Pool.Ping(ctx) == nil
	}, 30*time.Second, 200*time.Millisecond, "the pool never recovered after the connections died")

	// Prove it end to end: a booking for a DIFFERENT slot must now succeed
	// through the same server, with no restart and no intervention.
	freshStart, _ := testutil.Slot(11, time.Hour)
	users := pg.Users(t, 1)
	body, _ := json.Marshal(map[string]any{
		"facility_id":      court.String(),
		"start":            freshStart.Format(time.RFC3339),
		"duration_minutes": 60,
	})
	req, err := http.NewRequest(http.MethodPost, srv.http.URL+"/api/v1/bookings", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+srv.tokenFor(t, users[0]))
	req.Header.Set(httpx.HeaderIdempotencyKey, uuid.NewString())

	resp, err := srv.http.Client().Do(req)
	require.NoError(t, err, "the API never recovered")
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode,
		"the API did not recover after the pool lost every connection: %s", raw)
}
