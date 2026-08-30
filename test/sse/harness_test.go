// Package sse_test drives the live-update path end to end: a real booking
// commits to a real Postgres, a real outbox dispatcher drains it, a real Redis
// carries it, and a real HTTP client reads it off a real text/event-stream
// response.
//
// Nothing here is mocked, and the reason is the same one that governs the rest
// of this repo's tests. The properties under test are that Redis pub/sub has no
// backlog, that a non-blocking send to a full channel is what stops one stalled
// client freezing every other, and that losing Redis entirely changes nothing
// about whether a booking succeeds. A fake bus has none of those properties, so
// a mocked version of this suite would assert only that the Go code calls the Go
// code.
package sse_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/internal/live"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// testHeartbeat is the cadence the suite runs the stream at.
//
// The production interval is fifteen seconds (httpx.StreamHeartbeat) and it is
// asserted directly in TestSSE_HeartbeatEvery15s. Observing three of those would
// cost forty-five seconds per run, which is the kind of arithmetic that ends
// with somebody deleting the test — so the CADENCE is verified here at a scaled
// interval and the VALUE is verified as a constant.
const testHeartbeat = 80 * time.Millisecond

// quiet keeps the server's logging out of the test output.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

type stackCfg struct {
	heartbeat     time.Duration
	buffer        int
	rdb           *redis.Client
	runDispatcher bool
	runHub        bool

	// awaitHubReady waits for the Redis subscription before returning. False
	// only for the outage cases, where there is no subscription to wait for and
	// waiting would just be a slow way to fail.
	awaitHubReady bool

	// cacheTTL overrides facility.CacheTTL. Zero keeps production's five
	// seconds.
	cacheTTL time.Duration
}

type stackOpt func(*stackCfg)

// withHubBuffer shrinks the per-connection depth so a consumer can be made to
// fall behind without publishing thousands of events.
func withHubBuffer(n int) stackOpt {
	return func(c *stackCfg) { c.buffer = n }
}

// withHeartbeat overrides the SSE comment interval.
func withHeartbeat(d time.Duration) stackOpt {
	return func(c *stackCfg) { c.heartbeat = d }
}

// withDeadRedis points the whole stack — hub, publisher, cache and rate limiter
// alike — at an address nothing is listening on. That is what a Redis outage
// looks like from inside the service, and it is the setup for non-negotiable #3.
func withDeadRedis() stackOpt {
	return func(c *stackCfg) {
		c.rdb = testutil.DeadRedis()
		c.awaitHubReady = false
	}
}

// withCacheTTL stretches the availability cache lifetime.
//
// The invalidation test needs this and would otherwise be worthless: the grid is
// cached for five seconds in production, so a test that merely waits for the key
// to disappear passes whether the transition deleted it or the TTL expired it.
// With a TTL far longer than the test, a missing key can ONLY be the DEL.
func withCacheTTL(d time.Duration) stackOpt {
	return func(c *stackCfg) { c.cacheTTL = d }
}

// stack is a running API, a running hub, and a running outbox dispatcher.
type stack struct {
	server *httptest.Server
	pg     *testutil.PG
	rdb    *redis.Client
	hub    *live.Hub
	loc    *time.Location
}

// newStack starts the whole live-update path.
func newStack(t *testing.T, opts ...stackOpt) *stack {
	t.Helper()

	pg := testutil.Postgres(t)

	// Always started, and always flushed, even for the dead-Redis cases: the
	// container is shared across the package and leaving a previous test's keys
	// behind would make a cache-invalidation assertion pass for the wrong reason.
	live0 := testutil.Redis(t)

	cfg := &stackCfg{
		heartbeat:     testHeartbeat,
		buffer:        live.DefaultBuffer,
		rdb:           live0,
		runDispatcher: true,
		runHub:        true,
		awaitHubReady: true,
	}
	for _, o := range opts {
		o(cfg)
	}

	loc, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)

	apiCfg := &config.Config{
		DBURL:               pg.DSN,
		DBMaxConns:          20,
		AuthMode:            config.AuthModeDev,
		JWTSecret:           "test-secret",
		WriteQueueDepth:     config.DefaultWriteQueueDepth,
		WriteTimeout:        5 * time.Second,
		TZDisplay:           "Asia/Kolkata",
		RateLimitIPPerMin:   1000000,
		RateLimitUserPerMin: 1000000,
	}

	facilities := facility.NewRepo(pg.Pool)
	bookings := booking.NewService(pg.DB, facilities, loc)
	availability := facility.NewAvailability(pg.DB.Replica, cfg.rdb, apiCfg.TZDisplay, quiet())
	if cfg.cacheTTL > 0 {
		availability = availability.WithTTL(cfg.cacheTTL)
	}

	hub := live.NewHub(cfg.rdb, quiet()).WithBuffer(cfg.buffer)
	if cfg.runHub {
		runHub(t, hub, cfg.awaitHubReady)
	}

	if cfg.runDispatcher {
		// ListenDSN is the direct Postgres address, so the dispatcher is woken by
		// the COMMIT rather than by its ticker. Without it every assertion here
		// would be waiting out a poll interval, and the suite would be measuring
		// the ticker instead of the fan-out.
		startDispatcher(t, pg, outbox.Options{
			Logger:        quiet(),
			ListenDSN:     pg.DSN,
			Interval:      200 * time.Millisecond,
			SlotPublisher: live.NewPublisher(cfg.rdb, loc, quiet()),
		})
	}

	srv := httptest.NewServer(httpx.NewRouter(httpx.RouterDeps{
		Config:          apiCfg,
		DB:              pg.DB,
		Redis:           cfg.rdb,
		Bookings:        bookings,
		Facilities:      facilities,
		Availability:    availability,
		Live:            hub,
		StreamHeartbeat: cfg.heartbeat,
		Logger:          quiet(),
	}))
	t.Cleanup(srv.Close)

	return &stack{server: srv, pg: pg, rdb: cfg.rdb, hub: hub, loc: loc}
}

// runHub starts the hub and waits for its subscription to be acknowledged.
//
// Waiting is not optional. Redis pub/sub has NO BACKLOG: anything published
// before the SUBSCRIBE is confirmed is discarded rather than queued, so a test
// that publishes without this barrier is racing connection setup and will fail
// on a loaded machine rather than on a defect.
func runHub(t *testing.T, hub *live.Hub, awaitReady bool) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = hub.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("live hub did not stop within 10s of cancellation")
		}
	})

	if !awaitReady {
		return
	}
	select {
	case <-hub.Ready():
	case <-time.After(20 * time.Second):
		t.Fatal("live hub never established its Redis subscription")
	}
}

// startDispatcher runs an outbox dispatcher for the duration of the test.
//
// Cleanup cancels it AND waits, because these tests share one Postgres: a
// dispatcher still draining after its test ended would eat the next test's rows
// and fail it somewhere unrelated.
func startDispatcher(t *testing.T, pg *testutil.PG, opt outbox.Options) {
	t.Helper()

	d := outbox.NewDispatcher(pg.DB, opt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("outbox dispatcher did not stop within 15s of cancellation")
		}
	})

	select {
	case <-d.Ready():
	case <-time.After(20 * time.Second):
		t.Fatal("dispatcher never established its LISTEN subscription")
	}
}

// ---------------------------------------------------------------------------
// HTTP helpers

func (s *stack) login(t *testing.T, rollNo string) string {
	t.Helper()

	body, _ := json.Marshal(map[string]any{"roll_no": rollNo})
	resp, err := s.server.Client().Post(
		s.server.URL+"/api/v1/dev/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "dev login failed: %s", raw)

	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotEmpty(t, out.Token)
	return out.Token
}

type bookingResult struct {
	status int
	raw    []byte
}

func (r bookingResult) id(t *testing.T) uuid.UUID {
	t.Helper()
	var out struct {
		ID uuid.UUID `json:"id"`
	}
	require.NoError(t, json.Unmarshal(r.raw, &out), "body was: %s", r.raw)
	return out.ID
}

// book posts a real booking through the real router, middleware and all.
func (s *stack) book(t *testing.T, token string, facilityID uuid.UUID, start time.Time, mins int) bookingResult {
	t.Helper()

	body, _ := json.Marshal(map[string]any{
		"facility_id":      facilityID.String(),
		"start":            start.Format(time.RFC3339),
		"duration_minutes": mins,
	})

	req, err := http.NewRequest(http.MethodPost, s.server.URL+"/api/v1/bookings", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(httpx.HeaderIdempotencyKey, uuid.NewString())

	resp, err := s.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return bookingResult{status: resp.StatusCode, raw: raw}
}

func (s *stack) cancel(t *testing.T, token string, id uuid.UUID) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodDelete, s.server.URL+"/api/v1/bookings/"+id.String(), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(httpx.HeaderIdempotencyKey, uuid.NewString())

	resp, err := s.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// campusGrid fetches the availability grid, which is what warms avail:{date}.
func (s *stack) campusGrid(t *testing.T, token, date string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, s.server.URL+"/api/v1/availability?date="+date, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// ---------------------------------------------------------------------------
// SSE client

// frame is one parsed SSE record: either a comment (heartbeat) or a named event.
type frame struct {
	comment bool
	name    string
	data    string
}

// sseClient is a connected EventSource, in Go.
type sseClient struct {
	resp   *http.Response
	frames chan frame
	done   chan struct{}
}

// connect opens the stream and starts parsing it.
//
// The reader runs on its own goroutine and exits when the body is closed or the
// server ends the response, which is what makes Close deterministic — and what
// keeps the goroutine-leak test honest about measuring the SERVER rather than
// this helper.
func (s *stack) connect(t *testing.T, token, date string) *sseClient {
	t.Helper()

	url := s.server.URL + "/api/v1/stream"
	if date != "" {
		url += "?date=" + date
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.server.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	c := &sseClient{
		resp: resp,
		// Generous, so the reader never blocks and a test that stops consuming
		// is exercising the SERVER's buffer rather than this one.
		frames: make(chan frame, 512),
		done:   make(chan struct{}),
	}

	go c.read()
	t.Cleanup(c.Close)
	return c
}

// read parses the SSE wire format into frames until the stream ends.
func (c *sseClient) read() {
	defer close(c.done)
	defer close(c.frames)

	scanner := bufio.NewScanner(c.resp.Body)
	var pending frame

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case line == "":
			// Blank line terminates a record.
			if pending.comment || pending.data != "" {
				c.frames <- pending
			}
			pending = frame{}

		case strings.HasPrefix(line, ":"):
			c.frames <- frame{comment: true, data: strings.TrimSpace(line[1:])}

		case strings.HasPrefix(line, "event:"):
			pending.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))

		case strings.HasPrefix(line, "data:"):
			pending.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

// Close ends the connection and waits for the reader to finish.
func (c *sseClient) Close() {
	_ = c.resp.Body.Close()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
}

// nextEvent returns the next slot event, skipping heartbeat comments.
func (c *sseClient) nextEvent(t *testing.T, timeout time.Duration) live.Event {
	t.Helper()

	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				t.Fatal("stream ended before an event arrived")
			}
			if f.comment {
				continue
			}
			require.Equal(t, "slot", f.name, "unexpected event type")

			var ev live.Event
			require.NoError(t, json.Unmarshal([]byte(f.data), &ev), "data was: %s", f.data)
			return ev

		case <-deadline:
			t.Fatalf("timed out after %s waiting for a slot event", timeout)
		}
	}
}

// expectNoEvent asserts that nothing but heartbeats arrives within d.
func (c *sseClient) expectNoEvent(t *testing.T, d time.Duration) {
	t.Helper()

	deadline := time.After(d)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				return
			}
			if f.comment {
				continue
			}
			t.Fatalf("received an unexpected slot event: %s", f.data)
		case <-deadline:
			return
		}
	}
}

// countComments waits for n heartbeat comments and returns when it has them.
func (c *sseClient) countComments(t *testing.T, n int, timeout time.Duration) int {
	t.Helper()

	got := 0
	deadline := time.After(timeout)
	for got < n {
		select {
		case f, ok := <-c.frames:
			if !ok {
				t.Fatalf("stream ended after %d of %d comments", got, n)
			}
			if f.comment {
				got++
			}
		case <-deadline:
			t.Fatalf("timed out after %s with %d of %d comments", timeout, got, n)
		}
	}
	return got
}

// ---------------------------------------------------------------------------
// misc

// awaitSubscribers blocks until the hub has registered n connections for a date.
//
// This is the barrier every fan-out assertion needs. A client is registered
// server-side BEFORE its response headers are written, so waiting on the count
// proves the subscription exists rather than merely that the request was sent —
// and an event published into the gap would otherwise vanish, pub/sub having no
// backlog to recover it from.
func awaitSubscribers(t *testing.T, hub *live.Hub, date string, n int) {
	t.Helper()
	waitFor(t, 10*time.Second, fmt.Sprintf("%d subscribers on %s", n, date), func() bool {
		return hub.Subscribers(date) == n
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// tomorrowAt returns [start, end) at the given IST hour tomorrow, in UTC.
//
// Tomorrow rather than today so the suite never depends on the wall clock: an
// 18:00 slot is in the past for anyone running the tests in the evening, and
// validate rejects that with a 422 that would look like a broken stream.
func tomorrowAt(hour int) time.Time {
	now := time.Now().In(testutil.IST).AddDate(0, 0, 1)
	return time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, testutil.IST).UTC()
}

// dateOf renders the local date a start time belongs to — the channel name and
// the cache key both key on it.
func dateOf(start time.Time, loc *time.Location) string {
	return start.In(loc).Format("2006-01-02")
}

// slotOf renders the local HH:MM an event carries.
func slotOf(start time.Time, loc *time.Location) string {
	return start.In(loc).Format("15:04")
}

// awaitOutboxDrained blocks until nothing is left PENDING.
//
// The barrier a test needs when it wants an EARLIER transition to have been
// published and forgotten before it starts watching for a later one. Waiting on
// the bookings table instead would prove the row committed, not that the
// dispatcher had already published it — and the event would then arrive after
// the client connected and fail the assertion it was supposed to set up.
func awaitOutboxDrained(t *testing.T, pg *testutil.PG) {
	t.Helper()
	waitFor(t, 20*time.Second, "the outbox to drain", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var n int
		if err := pg.Pool.QueryRow(ctx,
			`SELECT count(*) FROM outbox WHERE status = 'PENDING'`).Scan(&n); err != nil {
			return false
		}
		return n == 0
	})
}

// exists reports whether a Redis key is present.
func exists(t *testing.T, rdb *redis.Client, key string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	n, err := rdb.Exists(ctx, key).Result()
	return err == nil && n == 1
}

func countBookings(t *testing.T, pg *testutil.PG, status string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var n int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE status = $1::booking_status`, status).Scan(&n))
	return n
}
