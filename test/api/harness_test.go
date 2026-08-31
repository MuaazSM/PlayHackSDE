// Package api_test drives the real router over real HTTP against real Postgres.
//
// httptest.Server rather than calling handlers directly: middleware order is
// load-bearing here, and a test that invokes the handler function bypasses every
// piece of it — auth, rate limiting, idempotency and shedding included.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
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
	client *http.Client
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
	availability := facility.NewAvailability(pg.DB.Replica, rdb, cfg.TZDisplay, nil)

	srv := httptest.NewServer(httpx.NewRouter(httpx.RouterDeps{
		Config:       cfg,
		DB:           pg.DB,
		Redis:        rdb,
		Bookings:     svc,
		Facilities:   facilities,
		Availability: availability,
	}))
	t.Cleanup(srv.Close)

	// A 500-way test can make many simultaneous loopback dials and transiently
	// overflow the Windows listener backlog, producing connectex refusals before
	// a request reaches the handler. Keep all callers and all request concurrency:
	// retry only failed connection setup a few times, and still return a final
	// error to the collecting goroutine if the listener really is unavailable.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 512
	transport.MaxIdleConnsPerHost = 512
	transport.MaxConnsPerHost = 512
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		var lastErr error
		for attempt := 0; attempt < 4; attempt++ {
			conn, err := dialer.DialContext(ctx, network, address)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if attempt == 3 {
				break
			}
			timer := time.NewTimer(time.Duration(1<<uint(attempt)) * 5 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return nil, ctx.Err()
			}
		}
		return nil, lastErr
	}
	client := &http.Client{Transport: transport}
	t.Cleanup(transport.CloseIdleConnections)

	return &api{server: srv, client: client, pg: pg, cfg: cfg}
}

// warmHTTP establishes n keep-alive connections to the test server before a
// race releases its goroutines.
//
// Five hundred goroutines released by one barrier means five hundred
// simultaneous loopback dials, and a Windows listener answers part of that SYN
// burst with a refusal — a connectex error that is a property of the burst, not
// of the application, and that used to fail this suite at exactly one request
// per run. Warming the pool in waves of 25 keeps every dial far below the
// backlog limit, and the race then reuses the idle connections instead of
// dialing. Waves rather than one serial loop, because 500 sequential requests
// cost more wall time than the race itself.
func (a *api) warmHTTP(t *testing.T, n int) {
	t.Helper()

	const wave = 25
	for established := 0; established < n; established += wave {
		size := min(wave, n-established)

		var (
			wg   sync.WaitGroup
			errs = make([]error, size)
		)
		for i := 0; i < size; i++ {
			wg.Add(1)
			go func(slot int) {
				defer wg.Done()
				resp, err := a.client.Get(a.server.URL + "/healthz")
				if err != nil {
					errs[slot] = err
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}(i)
		}
		wg.Wait()

		for _, err := range errs {
			require.NoError(t, err, "warming %d connections to the test server", n)
		}
	}
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

	resp, err := a.doResult(context.Background(), req)
	require.NoError(t, err)
	return resp
}

// doResult performs an HTTP request without testing.T assertions. Race workers
// must return failures to the collecting goroutine; calling require.NoError in
// a worker invokes FailNow outside the test goroutine and can leave a nil value
// that later panics during result collection.
func (a *api) doResult(ctx context.Context, req request) (response, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	var body io.Reader
	if req.body != nil {
		raw, err := json.Marshal(req.body)
		if err != nil {
			return response{}, err
		}
		body = bytes.NewReader(raw)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, a.server.URL+req.path, body)
	if err != nil {
		return response{}, err
	}
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

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}

	return response{status: resp.StatusCode, raw: raw, headers: resp.Header}, nil
}

// createBooking is the common write, as a real HTTP request.
func (a *api) createBooking(t *testing.T, token string, facilityID uuid.UUID, start time.Time, mins int, idemKey string) response {
	t.Helper()
	resp, err := a.createBookingResult(context.Background(), token, facilityID, start, mins, idemKey)
	require.NoError(t, err)
	return resp
}

// createBookingResult is the worker-safe form of createBooking.
func (a *api) createBookingResult(ctx context.Context, token string, facilityID uuid.UUID, start time.Time, mins int, idemKey string) (response, error) {
	return a.doResult(ctx, request{
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
