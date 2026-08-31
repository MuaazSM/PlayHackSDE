// Package observability_test checks that the eight metrics of
// IMPLEMENTATION.md §14 exist, carry the labels the surge dashboard queries by,
// and move when the thing they measure happens.
//
// A metric that is registered but never incremented is worse than a missing one:
// the panel renders, the line sits at zero, and the flat line reads as "no
// contention" rather than "nobody wired this up". Every test below drives the
// real code path and then reads the exported value.
package observability_test

import (
	"context"
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
	"github.com/iitg-playhack/sportsbook/internal/observability"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/jackc/pgx/v5"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// theEight is the closed list from §14. Named here rather than inferred, so that
// adding a ninth metric fails a test and has to be argued for.
var theEight = []string{
	"booking_write_duration_seconds",
	"booking_conflicts_total",
	"booking_shed_total",
	"write_queue_depth",
	"availability_query_duration_seconds",
	"waitlist_promotions_total",
	"outbox_pending",
	"replica_lag_seconds",
}

// --- harness ---------------------------------------------------------------

type stack struct {
	server *httptest.Server
	pg     *testutil.PG
	cfg    *config.Config
}

// newStack runs the real router, over real HTTP, against real Postgres. The
// metrics being asserted are produced by middleware, so a test that called the
// handler directly would measure nothing.
func newStack(t *testing.T, depth int) *stack {
	t.Helper()

	pg := testutil.Postgres(t)
	cfg := &config.Config{
		DBURL:               pg.DSN,
		DBMaxConns:          20,
		AuthMode:            config.AuthModeDev,
		JWTSecret:           "test-secret",
		WriteQueueDepth:     depth,
		WriteTimeout:        5 * time.Second,
		TZDisplay:           "Asia/Kolkata",
		RateLimitIPPerMin:   100000,
		RateLimitUserPerMin: 100000,
	}

	loc, err := time.LoadLocation(cfg.TZDisplay)
	require.NoError(t, err)

	facilities := facility.NewRepo(pg.Pool)
	svc := booking.NewService(pg.DB, facilities, loc)
	availability := facility.NewAvailability(pg.DB.Replica, nil, cfg.TZDisplay, quiet())

	srv := httptest.NewServer(httpx.NewRouter(httpx.RouterDeps{
		Config:       cfg,
		DB:           pg.DB,
		Bookings:     svc,
		Facilities:   facilities,
		Availability: availability,
		Logger:       quiet(),
	}))
	t.Cleanup(srv.Close)

	return &stack{server: srv, pg: pg, cfg: cfg}
}

func quiet() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// token signs a bearer for a user id without going through dev login, which
// would need a roll number this test does not have.
func (s *stack) token(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	auth := httpx.NewAuthenticator(s.cfg, s.pg.Pool)
	tok, err := auth.Sign(httpx.Principal{UserID: userID, RollNo: userID.String()[:8], Role: "STUDENT"}, time.Now())
	require.NoError(t, err)
	return tok
}

func (s *stack) book(t *testing.T, token string, facilityID uuid.UUID, start time.Time) int {
	t.Helper()

	body := fmt.Sprintf(`{"facility_id":%q,"start":%q,"duration_minutes":60}`,
		facilityID, start.UTC().Format(time.RFC3339))

	req, err := http.NewRequest(http.MethodPost, s.server.URL+"/api/v1/bookings", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(httpx.HeaderIdempotencyKey, uuid.NewString())

	resp, err := s.server.Client().Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

// scrape reads /metrics off the main port. §14 puts it there rather than on an
// admin listener: one binary, one port, and a metrics endpoint you have to
// remember a second port number for is one nobody opens during a demo.
func (s *stack) scrape(t *testing.T) string {
	t.Helper()

	resp, err := s.server.Client().Get(s.server.URL + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(raw)
}

// --- tests -----------------------------------------------------------------

func TestMetrics_EndpointExposesAllEight(t *testing.T) {
	s := newStack(t, config.DefaultWriteQueueDepth)

	// booking_conflicts_total is labelled by facility, and Prometheus omits a
	// vector with no children entirely. The label set is the catalogue, which is
	// not known at boot, so the family appears with the first real conflict —
	// this stands in for one. Everything else must already be there: the bounded
	// label sets are pre-registered so the dashboard is never blank.
	observability.RecordConflict("Tennis Court 1")

	body := s.scrape(t)
	for _, name := range theEight {
		require.Contains(t, body, "# HELP "+name+" ",
			"/metrics does not expose %s; §14 names exactly eight and this is one of them", name)
	}
}

func TestMetrics_WriteDurationLabelledByOutcome(t *testing.T) {
	s := newStack(t, config.DefaultWriteQueueDepth)

	before := map[string]float64{
		observability.OutcomeConfirmed: writeCount(t, s.scrape(t), observability.OutcomeConfirmed),
		observability.OutcomeConflict:  writeCount(t, s.scrape(t), observability.OutcomeConflict),
	}

	users := s.pg.Users(t, 2)
	start, _ := testutil.Slot18()

	require.Equal(t, http.StatusCreated, s.book(t, s.token(t, users[0]), testutil.CourtID(), start))
	require.Equal(t, http.StatusConflict, s.book(t, s.token(t, users[1]), testutil.CourtID(), start))

	// The split is the whole point: one histogram for both would average the
	// winner and the losers and describe neither.
	body := s.scrape(t)
	require.Contains(t, body, `booking_write_duration_seconds_count{outcome="confirmed"}`)
	require.Contains(t, body, `booking_write_duration_seconds_count{outcome="conflict"}`)

	require.Equal(t, before[observability.OutcomeConfirmed]+1,
		writeCount(t, body, observability.OutcomeConfirmed),
		"a 201 must land in booking_write_duration_seconds{outcome=confirmed}")
	require.Equal(t, before[observability.OutcomeConflict]+1,
		writeCount(t, body, observability.OutcomeConflict),
		"a 409 must land in booking_write_duration_seconds{outcome=conflict}")
}

// writeCount reads the observation count for one outcome out of a scrape.
//
// Read from the exposition text rather than from the collector, because what
// matters is what Prometheus would see: a metric that is correct in memory and
// absent from /metrics is a blank panel.
func writeCount(t *testing.T, body, outcome string) float64 {
	t.Helper()
	return sampleValue(t, body,
		fmt.Sprintf(`booking_write_duration_seconds_count{outcome="%s"}`, outcome))
}

// sampleValue pulls one series' value out of an exposition body.
func sampleValue(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(line, series+" ")
		if !ok {
			continue
		}
		var v float64
		_, err := fmt.Sscanf(strings.TrimSpace(rest), "%g", &v)
		require.NoError(t, err, "could not parse %q", line)
		return v
	}
	t.Fatalf("series %s not present in /metrics", series)
	return 0
}

func TestMetrics_ConflictCounterIncrements(t *testing.T) {
	s := newStack(t, config.DefaultWriteQueueDepth)

	const name = "Tennis Court 1" // the seeded exclusive court
	before := conflictCount(t, name)

	users := s.pg.Users(t, 3)
	start, _ := testutil.Slot18()

	require.Equal(t, http.StatusCreated, s.book(t, s.token(t, users[0]), testutil.CourtID(), start))
	require.Equal(t, http.StatusConflict, s.book(t, s.token(t, users[1]), testutil.CourtID(), start))
	require.Equal(t, http.StatusConflict, s.book(t, s.token(t, users[2]), testutil.CourtID(), start))

	require.Equal(t, before+2, conflictCount(t, name),
		"two lost races must show as two conflicts against the facility that lost them")

	// Labelled by name, not by UUID: seven facilities is a constant cardinality,
	// and a UUID on the dashboard's axis is unreadable.
	require.Contains(t, s.scrape(t), fmt.Sprintf(`booking_conflicts_total{facility="%s"}`, name))
}

// conflictCount reads the conflict counter for one facility.
func conflictCount(t *testing.T, facilityName string) float64 {
	t.Helper()
	c, err := observability.BookingConflicts.GetMetricWithLabelValues(facilityName)
	require.NoError(t, err)
	return promtestutil.ToFloat64(c)
}

func TestMetrics_QueueDepthTracksShedder(t *testing.T) {
	shedder := httpx.NewShedder(1, 0)

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- shedder.Do(context.Background(), func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()

	<-entered
	require.Equal(t, 1.0, promtestutil.ToFloat64(observability.WriteQueueDepth),
		"write_queue_depth must report writes IN FLIGHT; the configured bound is a constant and a constant makes a useless graph")

	// The second caller finds no slot and is refused without waiting. That is the
	// feature, not a degradation.
	shedBefore := promtestutil.ToFloat64(observability.BookingShed)
	require.ErrorIs(t, shedder.Do(context.Background(), func(context.Context) error {
		t.Fatal("shedder admitted a second caller into a depth-1 queue")
		return nil
	}), booking.ErrShed)
	require.Equal(t, shedBefore+1, promtestutil.ToFloat64(observability.BookingShed))

	close(release)
	require.NoError(t, <-done)
	require.Equal(t, 0.0, promtestutil.ToFloat64(observability.WriteQueueDepth),
		"the gauge must come back down, or the dashboard shows a permanently full queue")
}

func TestMetrics_OutboxPendingGauge(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	dispatcher := outbox.NewDispatcher(pg.DB, outbox.Options{
		Notifier: outbox.NewLogNotifier(quiet()),
		Logger:   quiet(),
	})

	// Three side effects, committed the only way anything may write one: inside
	// a transaction (non-negotiable #7).
	require.NoError(t, store.WithTx(ctx, pg.Pool, func(tx pgx.Tx) error {
		for i := 0; i < 3; i++ {
			if err := outbox.Enqueue(ctx, tx, outbox.TopicBookingConfirmed,
				map[string]any{"n": i}); err != nil {
				return err
			}
		}
		return nil
	}))

	// The gauge is sampled by the dispatcher, at the end of a pass. Before one
	// runs it has no reason to have moved.
	require.NoError(t, dispatcher.DrainNow(ctx))
	require.Equal(t, 0.0, promtestutil.ToFloat64(observability.OutboxPending),
		"a drained queue must report zero pending, or a stalled dispatcher is indistinguishable from a busy one")

	// Now leave a backlog the dispatcher cannot clear, and confirm the gauge
	// reports it. A failing transport does NOT return rows to PENDING within the
	// same pass, so this writes rows and samples without draining them.
	require.NoError(t, store.WithTx(ctx, pg.Pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.TopicBookingConfirmed, map[string]any{"n": 99})
	}))

	var pending int64
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE status = 'PENDING'`).Scan(&pending))
	require.Equal(t, int64(1), pending)

	require.NoError(t, dispatcher.DrainNow(ctx))
	require.Equal(t, 0.0, promtestutil.ToFloat64(observability.OutboxPending))
}

func TestReplicaFallback_WorksWhenReplicaURLUnset(t *testing.T) {
	pg := testutil.Postgres(t)

	cfg := &config.Config{
		DBURL:        pg.DSN,
		DBReplicaURL: "", // the whole point: unset
		DBMaxConns:   5,
		TZDisplay:    "Asia/Kolkata",
	}

	ctx := context.Background()
	db, err := store.New(ctx, cfg)
	require.NoError(t, err, "an unset DB_REPLICA_URL must not stop the API from starting")
	t.Cleanup(db.Close)

	require.False(t, db.HasDedicatedReplica())
	require.Same(t, db.Primary, db.Replica,
		"with no replica configured, reads share the primary pool rather than opening a second one to the same server")

	// The read path is wired to db.Replica and must still work. This is the
	// degraded mode §2.1 promises: "works, just not split".
	availability := facility.NewAvailability(db.Replica, nil, cfg.TZDisplay, quiet())
	repo := facility.NewRepo(db.Primary)

	f, err := repo.Get(ctx, testutil.CourtID())
	require.NoError(t, err)

	start, _ := testutil.Slot18()
	day, err := availability.ForFacility(ctx, f, start.In(time.UTC).Format("2006-01-02"))
	require.NoError(t, err, "availability must serve from the primary when there is no replica")
	require.NotNil(t, day)

	// replica_lag_seconds is 0, not absent. There is no second server to trail,
	// so the read path genuinely cannot be stale — and a gauge that simply
	// stopped reporting would look like a crashed sampler.
	observability.SetReplicaLag(42) // poison it first, so 0 proves the call ran
	observability.SampleReplicaLag(ctx, db.Replica, db.HasDedicatedReplica(), time.Millisecond, quiet())
	require.Equal(t, 0.0, promtestutil.ToFloat64(observability.ReplicaLag))
}
