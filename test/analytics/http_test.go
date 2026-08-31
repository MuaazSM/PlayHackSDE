package analytics_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/analytics"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// serve starts the REAL router, not the handler.
//
// The role gate is middleware, and a test that calls the handler function
// directly would bypass the one thing it is checking.
func serve(t *testing.T, pg *testutil.PG) *httptest.Server {
	t.Helper()

	cfg := &config.Config{
		DBURL:               pg.DSN,
		DBMaxConns:          20,
		AuthMode:            config.AuthModeDev,
		JWTSecret:           "test-secret",
		WriteQueueDepth:     config.DefaultWriteQueueDepth,
		WriteTimeout:        5 * time.Second,
		TZDisplay:           tz,
		RateLimitIPPerMin:   100000,
		RateLimitUserPerMin: 100000,
	}

	loc, err := time.LoadLocation(cfg.TZDisplay)
	require.NoError(t, err)

	facilities := facility.NewRepo(pg.Pool)
	srv := httptest.NewServer(httpx.NewRouter(httpx.RouterDeps{
		Config:       cfg,
		DB:           pg.DB,
		Bookings:     booking.NewService(pg.DB, facilities, loc),
		Facilities:   facilities,
		Availability: facility.NewAvailability(pg.DB.Replica, nil, cfg.TZDisplay, nil),
		Analytics:    analytics.NewService(pg.DB.Replica, nil, cfg.TZDisplay, nil),
	}))
	t.Cleanup(srv.Close)
	return srv
}

func login(t *testing.T, srv *httptest.Server, roll string) string {
	t.Helper()

	body := `{"roll_no":"` + roll + `"}`
	resp, err := srv.Client().Post(srv.URL+"/api/v1/dev/login", "application/json",
		strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotEmpty(t, out.Token)
	return out.Token
}

func getAnalytics(t *testing.T, srv *httptest.Server, token string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/api/v1/admin/analytics?from="+day1+"&to="+day2, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

// TestAnalytics_RequiresManagerOrSecretary.
//
// The report names which students failed to turn up and how oversubscribed each
// facility is. That is staff information — a student reading it learns other
// students' attendance records — so the gate is 403, not a thinner payload.
//
// SECRETARY is admitted alongside MANAGER because the sports secretary is a
// student office-holder who plans the timetable from exactly these numbers.
// They are still not admitted to /closures, which is why this route uses
// RequireAnyRole and that one still uses RequireRole(MANAGER).
func TestAnalytics_RequiresManagerOrSecretary(t *testing.T) {
	pg := testutil.Postgres(t)
	srv := serve(t, pg)

	t.Run("manager is admitted", func(t *testing.T) {
		status, body := getAnalytics(t, srv, login(t, srv, "manager01"))
		require.Equal(t, http.StatusOK, status, "body: %s", body)

		var rep analytics.Report
		require.NoError(t, json.Unmarshal(body, &rep))
		require.Equal(t, day1, rep.From)
		require.Equal(t, day2, rep.To)
		require.Len(t, rep.PeakDemand.Cells, 7)
	})

	t.Run("secretary is admitted", func(t *testing.T) {
		status, body := getAnalytics(t, srv, login(t, srv, "secretary01"))
		require.Equal(t, http.StatusOK, status, "body: %s", body)
	})

	t.Run("student is refused", func(t *testing.T) {
		status, body := getAnalytics(t, srv, login(t, srv, "student01"))
		require.Equal(t, http.StatusForbidden, status)

		// One error envelope, machine-readable code (§10.3).
		var env struct {
			Error string `json:"error"`
		}
		require.NoError(t, json.Unmarshal(body, &env))
		require.NotEmpty(t, env.Error)
	})

	t.Run("anonymous is refused", func(t *testing.T) {
		status, _ := getAnalytics(t, srv, "")
		require.Equal(t, http.StatusUnauthorized, status)
	})

	t.Run("a malformed range is a 400, not a 500", func(t *testing.T) {
		token := login(t, srv, "manager01")
		req, err := http.NewRequest(http.MethodGet,
			srv.URL+"/api/v1/admin/analytics?from=yesterday&to="+day2, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}
