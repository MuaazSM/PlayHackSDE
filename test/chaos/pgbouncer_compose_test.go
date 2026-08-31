package chaos_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestComposePgBouncerRestart is the opt-in, real pooler counterpart to the
// fast connection-loss test in chaos_test.go. It restarts the actual compose
// container while requests are in flight and proves the existing pgx pool
// reconnects without allowing a second winner or a partial commit.
func TestComposePgBouncerRestart(t *testing.T) {
	dsn := os.Getenv("CHAOS_PGBOUNCER_URL")
	if dsn == "" {
		t.Skip("set CHAOS_PGBOUNCER_URL to run the real compose PgBouncer restart")
	}
	container := os.Getenv("CHAOS_PGBOUNCER_CONTAINER")
	if container == "" {
		container = "playhack-pgbouncer"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := store.New(ctx, &config.Config{DBURL: dsn, DBMaxConns: 25})
	require.NoError(t, err)
	t.Cleanup(db.Close)

	pg := &testutil.PG{DB: db, Pool: db.Primary, DSN: dsn}
	pg.Warm(t, 25)
	srv := newServer(t, pg, nil)
	defer srv.close()

	court := testutil.CourtID()
	start, end := testutil.Slot18()
	var restartErr error
	results := storm(t, srv, 200, court, start, func() {
		out, cmdErr := exec.CommandContext(ctx, "docker", "restart", container).CombinedOutput()
		if cmdErr != nil {
			restartErr = fmt.Errorf("docker restart %s: %w: %s", container, cmdErr, out)
		}
	})
	require.NoError(t, restartErr)

	tally := count(results)
	t.Logf("real PgBouncer restart outcome: %+v", tally)
	require.LessOrEqual(t, confirmed(t, pg, court, start, end), 1, "double booking across real pooler restart")

	var orphans int
	require.NoError(t, pg.Pool.QueryRow(ctx, `
		SELECT count(*) FROM bookings b
		 WHERE b.status = 'CONFIRMED'
		   AND NOT EXISTS (
		     SELECT 1 FROM booking_events e
		      WHERE e.booking_id = b.id AND e.to_status = 'CONFIRMED'
	   )`).Scan(&orphans))
	require.Zero(t, orphans, "pooler restart committed a booking without its audit event")

	require.Eventually(t, func() bool {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer pingCancel()
		return pg.Pool.Ping(pingCtx) == nil
	}, 30*time.Second, 200*time.Millisecond, "pgx pool did not reconnect after PgBouncer restarted")

	// A different slot must remain writable through the same process and pool.
	freshStart, freshEnd := testutil.Slot(11, time.Hour)
	second := storm(t, srv, 20, court, freshStart, nil)
	require.Equal(t, 1, confirmed(t, pg, court, freshStart, freshEnd),
		"API did not recover through the restarted pooler: %+v", count(second))
}
