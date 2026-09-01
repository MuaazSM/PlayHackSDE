package main

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// loadProfileDepth is the WRITE_QUEUE_DEPTH the profile runs at, and the same
// number the Makefile pins. Measured, not assumed — the sweep and the reasoning
// are in the Makefile next to the value.
const loadProfileDepth = 24

// TestLoadProfile_ThresholdsPass is `make load` as a test.
//
// It runs the same profile the Makefile target does, against the same real
// router over real HTTP, and applies the same three failing thresholds. The
// point of having it here as well as in the Makefile is that a change which
// quietly slows the write path should fail somebody's build, not wait to be
// noticed on stage.
//
// Under -race the two percentile assertions are dropped and everything else is
// kept: the detector inflates per-request cost several-fold, so a p99 measured
// under it is a number about the detector. `make load` builds without it.
func TestLoadProfile_ThresholdsPass(t *testing.T) {
	if testing.Short() {
		t.Skip("load profile needs a real Postgres and 500 concurrent requests")
	}

	const n = 500

	pg := testutil.Postgres(t)
	cfg := &config.Config{
		DBURL:               pg.DSN,
		DBMaxConns:          25,
		AuthMode:            config.AuthModeDev,
		JWTSecret:           "test-secret",
		WriteQueueDepth:     loadProfileDepth,
		WriteTimeout:        5 * time.Second,
		TZDisplay:           "Asia/Kolkata",
		RateLimitIPPerMin:   1000000,
		RateLimitUserPerMin: 1000000,
	}

	loc, err := time.LoadLocation(cfg.TZDisplay)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// serveInProcess replaces the default logger; put it back afterwards so a
	// later test in this package is not silently muted.
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) })

	stop, base, err := serveInProcess(ctx, cfg, pg.DB, loc)
	require.NoError(t, err)
	t.Cleanup(stop)

	tokens, err := mintTokens(ctx, cfg, pg.Pool, n)
	require.NoError(t, err)

	start, _ := testutil.Slot18()
	runner := &Runner{
		Client:     newHTTPClient(n, 60*time.Second),
		BaseURL:    base,
		FacilityID: testutil.CourtID(), // exclusive: exactly one winner
		Start:      start,
		Minutes:    60,
		Tokens:     tokens,
	}

	rep, err := runner.Run(ctx, n)
	require.NoError(t, err)

	// The printed report must reflect the checks that actually run. Latency
	// budgets are a declared-hardware-profile assertion: the race detector
	// multiplies per-request cost, Docker Desktop on Windows routes every query
	// through the WSL2 vsock, and a shared CI runner is two vCPUs of
	// variable-tenancy hardware — none of those machines can honestly measure
	// a p99 the budget was set against. Correctness and transport invariants
	// apply everywhere; `make load` on the profile the Makefile's depth table
	// was measured on enforces the budgets.
	budgetsApply := !raceEnabled && runtime.GOOS != "windows" && os.Getenv("CI") != "true"
	if budgetsApply {
		rep.Check(true)
	} else {
		rep.CheckWithoutLatency(true)
	}
	rep.Print(os.Stdout, true)

	// --- the invariant, always ---------------------------------------------
	//
	// Checked first and unconditionally. A run where every request failed would
	// satisfy both latency budgets beautifully, so the budgets mean nothing
	// without this.
	require.Equal(t, 0, rep.ServerErrors(), "a 5xx is the system not knowing what happened")
	require.Equal(t, 0, rep.Errors, "transport errors")
	require.Equal(t, 1, rep.Confirmed(),
		"exactly one of %d requests may win one exclusive slot", n)
	require.Positive(t, rep.Conflicts(), "the run created no contention; there is nothing to measure")

	// The database is the authority on that, not the response codes.
	var confirmed int
	require.NoError(t, pg.Pool.QueryRow(ctx, `
		SELECT count(*) FROM bookings
		 WHERE facility_id = $1
		   AND status = 'CONFIRMED'
		   AND during && tstzrange($2, $3, '[)')`,
		testutil.CourtID(), start, start.Add(time.Hour)).Scan(&confirmed))
	require.Equal(t, 1, confirmed, "SELECT count(*) FROM bookings must be 1")

	// --- the budgets --------------------------------------------------------
	if !budgetsApply {
		t.Logf("p99 budgets not enforced in this environment "+
			"(409 p99 %s, 201 p99 %s observed; enforced on the declared hardware profile)",
			rep.ConflictP99, rep.ConfirmP99)
		return
	}

	require.Less(t, rep.ConflictP99, ConflictP99Budget,
		"losing must be faster than winning: 409 p99")
	require.Less(t, rep.ConfirmP99, ConfirmedP99Budget, "201 p99")
	require.Empty(t, rep.Failures)
}
