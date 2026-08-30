package checkin_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/checkin"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// The two endpoints of §7, over the REAL router.
//
// httptest.Server rather than calling the handlers directly, for the reason the
// api suite gives: middleware order is load-bearing, and the manager-only gate on
// the token route IS middleware. A test that invoked the handler function would
// prove the token is minted and prove nothing about who may ask for it.

// ciAPI is a running server plus the rig behind it.
type ciAPI struct {
	server *httptest.Server
	rig    *ciRig
}

func newCIAPI(t *testing.T, pg *testutil.PG) *ciAPI {
	t.Helper()

	r := newCIRig(t, pg).openCheckIn()

	cfg := &config.Config{
		DBURL:               pg.DSN,
		DBMaxConns:          20,
		AuthMode:            config.AuthModeDev,
		JWTSecret:           "test-secret",
		CheckinHMACSecret:   testSecret,
		WriteQueueDepth:     config.DefaultWriteQueueDepth,
		WriteTimeout:        5 * time.Second,
		TZDisplay:           "Asia/Kolkata",
		RateLimitIPPerMin:   100000,
		RateLimitUserPerMin: 100000,
	}

	srv := httptest.NewServer(httpx.NewRouter(httpx.RouterDeps{
		Config:       cfg,
		DB:           pg.DB,
		Bookings:     r.bookings,
		Facilities:   r.cat,
		Availability: facility.NewAvailability(pg.DB.Replica, nil, cfg.TZDisplay, ciQuiet()),
		Waitlist:     r.queue,
		// The service with the widened check-in window, so a booking for
		// tomorrow is inside it. Supplied rather than left nil for a second
		// reason worth stating: it is the one the sweeper would also hold, which
		// is how cmd/api wires it.
		Checkin: r.svc,
		Logger:  ciQuiet(),
	}))
	t.Cleanup(srv.Close)

	return &ciAPI{server: srv, rig: r}
}

func (a *ciAPI) login(t *testing.T, rollNo string) string {
	t.Helper()
	resp, body := a.do(t, http.MethodPost, "/api/v1/dev/login", "", map[string]any{"roll_no": rollNo})
	require.Equal(t, http.StatusOK, resp, "dev login failed: %s", body)

	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.Token
}

func (a *ciAPI) do(t *testing.T, method, path, token string, payload any) (int, []byte) {
	t.Helper()

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, a.server.URL+path, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
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

// TestAPI_CheckinToken_ManagerOnly is the access control on the venue display.
//
// The token is a bearer proof of standing at the venue. Serving it to a student
// over the API would let anybody check in from anywhere, and the no-show release
// — the whole feature — would stop meaning anything. So the route is
// manager-only, enforced by middleware rather than by a check inside the handler
// that somebody could later move.
func TestAPI_CheckinToken_ManagerOnly(t *testing.T) {
	pg := testutil.Postgres(t)
	a := newCIAPI(t, pg)

	path := "/api/v1/facilities/" + a.rig.court.String() + "/checkin-token"

	status, body := a.do(t, http.MethodGet, path, a.login(t, "student01"), nil)
	require.Equal(t, http.StatusForbidden, status, "body was: %s", body)

	status, body = a.do(t, http.MethodGet, path, a.login(t, "manager01"), nil)
	require.Equal(t, http.StatusOK, status, "body was: %s", body)

	var out struct {
		FacilityID       uuid.UUID `json:"facility_id"`
		Token            string    `json:"token"`
		RefreshInSeconds int       `json:"refresh_in_seconds"`
		ValidForSeconds  int       `json:"valid_for_seconds"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.Equal(t, a.rig.court, out.FacilityID)
	require.NotEmpty(t, out.Token)

	// The display refreshes on the first number and the code keeps working for
	// one window past it, which is where the scanning tolerance comes from.
	require.Greater(t, out.RefreshInSeconds, 0)
	require.Greater(t, out.ValidForSeconds, out.RefreshInSeconds)

	// And it really is the token the verifier expects — a route that minted with
	// a different secret would look identical from here.
	require.True(t, a.rig.minter.Verify(out.Token, a.rig.court))

	// Unauthenticated is 401, not 403: the caller has not said who they are.
	status, _ = a.do(t, http.MethodGet, path, "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
}

// TestAPI_CheckIn_EndToEnd walks the student's half over HTTP: scan, tap, done —
// and tap again, which must be harmless.
func TestAPI_CheckIn_EndToEnd(t *testing.T) {
	pg := testutil.Postgres(t)
	a := newCIAPI(t, pg)

	student := testutil.UserIDByRoll("student01")
	token := a.login(t, "student01")

	start, _ := ciSlot(15, time.Hour)
	b := a.rig.book(t, a.rig.court, student, start, time.Hour)
	path := "/api/v1/bookings/" + b.ID.String() + "/check-in"

	// The code on the wall, as the manager's display would be showing it.
	qr := a.rig.token(a.rig.court)

	status, body := a.do(t, http.MethodPost, path, token, map[string]any{"token": qr})
	require.Equal(t, http.StatusOK, status, "body was: %s", body)

	var out struct {
		BookingID   uuid.UUID `json:"booking_id"`
		Reference   string    `json:"reference"`
		CheckedInAt time.Time `json:"checked_in_at"`
		Method      string    `json:"method"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.Equal(t, b.ID, out.BookingID)
	require.Equal(t, "QR", out.Method)

	// A second tap is a 200 with the same record, not a conflict. The primary key
	// on check_ins does that, with no Idempotency-Key header in sight.
	status, body = a.do(t, http.MethodPost, path, token, map[string]any{"token": qr})
	require.Equal(t, http.StatusOK, status, "body was: %s", body)

	var again struct {
		CheckedInAt time.Time `json:"checked_in_at"`
	}
	require.NoError(t, json.Unmarshal(body, &again))
	require.True(t, out.CheckedInAt.Equal(again.CheckedInAt))
	require.Equal(t, 1, ciCheckInCount(t, pg))

	// A forged code is a 403 in the one error envelope, with the machine-readable
	// code the client switches on.
	status, body = a.do(t, http.MethodPost, path, token, map[string]any{"token": "not-a-real-token"})
	require.Equal(t, http.StatusForbidden, status)

	var envelope httpx.ErrorBody
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Equal(t, httpx.CodeCheckinToken, envelope.Error)
	require.NotEmpty(t, envelope.RequestID)
}

// TestAPI_CheckIn_OutsideWindowConflict pins the status code the client branches
// on when a student taps too early. 409 rather than 422: the request is
// perfectly well formed, it is the state of the world that refuses it.
func TestAPI_CheckIn_OutsideWindowConflict(t *testing.T) {
	pg := testutil.Postgres(t)
	a := newCIAPI(t, pg)

	// Back to the production window, so tomorrow's slot is genuinely too early.
	// The router holds this very service, so the change reaches the handler.
	a.rig.svc.WithWindow(checkin.DefaultEarlyWindow, checkin.DefaultGracePeriod)

	student := testutil.UserIDByRoll("student01")
	token := a.login(t, "student01")

	start, _ := ciSlot(15, time.Hour)
	b := a.rig.book(t, a.rig.court, student, start, time.Hour)

	status, body := a.do(t, http.MethodPost,
		"/api/v1/bookings/"+b.ID.String()+"/check-in", token,
		map[string]any{"token": a.rig.token(a.rig.court)})
	require.Equal(t, http.StatusConflict, status)

	var envelope httpx.ErrorBody
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Equal(t, httpx.CodeCheckinWindow, envelope.Error)

	require.Equal(t, 0, ciCheckInCount(t, pg))
}
