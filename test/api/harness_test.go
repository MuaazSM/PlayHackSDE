// Package api_test drives the real router over real HTTP against real Postgres.
//
// httptest.Server rather than calling handlers directly: middleware order is
// load-bearing here, and a test that invokes the handler function bypasses every
// piece of it — auth, rate limiting, idempotency and shedding included.
package api_test

import (
	"bytes"
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
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// api is a running server plus the bits a test needs to talk to it.
type api struct {
	server *httptest.Server
	pg     *testutil.PG
	cfg    *config.Config
}

type option func(*config.Config, *redis.Client) *redis.Client

// withDepth forces the write queue bound, for the shed tests.
func withDepth(n int) option {
	return func(c *config.Config, r *redis.Client) *redis.Client {
		c.WriteQueueDepth = n
		return r
	}
}

// withRedis attaches a live Redis, enabling rate limiting.
func withRedis(rdb *redis.Client) option {
	return func(_ *config.Config, _ *redis.Client) *redis.Client { return rdb }
}

// withUserLimit sets the per-user budget.
func withUserLimit(n int) option {
	return func(c *config.Config, r *redis.Client) *redis.Client {
		c.RateLimitUserPerMin = n
		return r
	}
}

// withIPLimit sets the pre-auth budget.
func withIPLimit(n int) option {
	return func(c *config.Config, r *redis.Client) *redis.Client {
		c.RateLimitIPPerMin = n
		return r
	}
}

// withDeadRedis points the limiter at an address nothing is listening on, which
// is what a Redis outage looks like from in here.
func withDeadRedis() option {
	return func(_ *config.Config, _ *redis.Client) *redis.Client {
		return redis.NewClient(&redis.Options{
			Addr:        "127.0.0.1:1",
			DialTimeout: 50 * time.Millisecond,
			MaxRetries:  -1,
		})
	}
}

// newAPI starts the real router.
func newAPI(t *testing.T, opts ...option) *api {
	t.Helper()

	pg := testutil.Postgres(t)

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

	var rdb *redis.Client
	for _, o := range opts {
		rdb = o(cfg, rdb)
	}

	loc, err := time.LoadLocation(cfg.TZDisplay)
	require.NoError(t, err)

	facilities := facility.NewRepo(pg.Pool)
	svc := booking.NewService(pg.DB, facilities, loc)

	srv := httptest.NewServer(httpx.NewRouter(httpx.RouterDeps{
		Config:     cfg,
		DB:         pg.DB,
		Redis:      rdb,
		Bookings:   svc,
		Facilities: facilities,
	}))
	t.Cleanup(srv.Close)

	return &api{server: srv, pg: pg, cfg: cfg}
}

// login exchanges a roll number for a bearer token through the real dev route.
func (a *api) login(t *testing.T, rollNo string) string {
	t.Helper()

	resp := a.do(t, request{
		method: http.MethodPost,
		path:   "/api/v1/dev/login",
		body:   map[string]any{"roll_no": rollNo},
	})
	require.Equal(t, http.StatusOK, resp.status, "dev login failed: %s", resp.raw)

	var body struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(resp.raw, &body))
	require.NotEmpty(t, body.Token)
	return body.Token
}

type request struct {
	method  string
	path    string
	token   string
	idemKey string
	body    any
	// omitIdemKey sends no Idempotency-Key even on a write.
	omitIdemKey bool
	rawIdemKey  string
}

type response struct {
	status  int
	raw     []byte
	headers http.Header
}

func (r response) decode(t *testing.T, into any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.raw, into), "body was: %s", r.raw)
}

func (r response) errorBody(t *testing.T) httpx.ErrorBody {
	t.Helper()
	var e httpx.ErrorBody
	r.decode(t, &e)
	return e
}

func (a *api) do(t *testing.T, req request) response {
	t.Helper()

	var body io.Reader
	if req.body != nil {
		raw, err := json.Marshal(req.body)
		require.NoError(t, err)
		body = bytes.NewReader(raw)
	}

	httpReq, err := http.NewRequest(req.method, a.server.URL+req.path, body)
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	if req.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.token)
	}
	switch {
	case req.rawIdemKey != "":
		httpReq.Header.Set(httpx.HeaderIdempotencyKey, req.rawIdemKey)
	case req.omitIdemKey:
	case req.idemKey != "":
		httpReq.Header.Set(httpx.HeaderIdempotencyKey, req.idemKey)
	default:
		httpReq.Header.Set(httpx.HeaderIdempotencyKey, uuid.NewString())
	}

	resp, err := a.server.Client().Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return response{status: resp.StatusCode, raw: raw, headers: resp.Header}
}

// createBooking is the common write, as a real HTTP request.
func (a *api) createBooking(t *testing.T, token string, facilityID uuid.UUID, start time.Time, mins int, idemKey string) response {
	t.Helper()
	return a.do(t, request{
		method:  http.MethodPost,
		path:    "/api/v1/bookings",
		token:   token,
		idemKey: idemKey,
		body: map[string]any{
			"facility_id":      facilityID.String(),
			"start":            start.Format(time.RFC3339),
			"duration_minutes": mins,
		},
	})
}
