package availability_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestCampusGrid_OneQuery is a hard requirement, not a preference (FR-02, G-1).
//
// A query per facility would make the discovery screen's cost scale with the
// catalogue and turn one slow read into seven. Counting the queries is the only
// way to assert it — the response looks identical either way.
func TestCampusGrid_OneQuery(t *testing.T) {
	pg := testutil.Postgres(t)
	pool, counter := pg.CountingPool(t)
	av := facility.NewAvailability(pool, nil, "Asia/Kolkata", nil)

	// Warm the pool so connection setup is not counted.
	require.NoError(t, pool.Ping(context.Background()))
	counter.Reset()

	grid, err := av.Campus(context.Background(), today())
	require.NoError(t, err)
	require.NotEmpty(t, grid.Facilities)

	require.Equal(t, int64(1), counter.Count(),
		"the campus grid must be ONE query; %d were issued, which is N+1 across facilities",
		counter.Count())
}

func TestCampusGrid_ShapeMatchesContract(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	av, _ := newAvailability(t, pg)
	ctx := context.Background()

	start, _ := testutil.Slot18()
	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: testutil.StudentID(0),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	grid, err := av.Campus(ctx, today())
	require.NoError(t, err)

	require.Equal(t, today(), grid.Date)
	require.Len(t, grid.Facilities, 7, "every active facility is a row")
	require.NotEmpty(t, grid.Slots)

	// The grid is a dense rectangle: one row per facility, one column per slot,
	// so the client indexes grid[facility][slot] without bounds checks.
	require.Len(t, grid.Grid, len(grid.Facilities))
	for i, row := range grid.Grid {
		require.Lenf(t, row, len(grid.Slots),
			"row %d (%s) is ragged", i, grid.Facilities[i].Name)
		for j, state := range row {
			require.NotEmptyf(t, state, "cell [%d][%d] is empty", i, j)
			require.Containsf(t, []string{
				facility.StateFree, facility.StateHeld, facility.StateBooked,
				facility.StateClosed, facility.StateFilling, facility.StateFull,
			}, state, "unknown state %q", state)
		}
	}

	// Slots are ordered and contiguous.
	for i := 1; i < len(grid.Slots); i++ {
		require.Equal(t, grid.Slots[i-1].End, grid.Slots[i].Start)
	}

	// The axis spans the union of opening hours: the gym opens earliest (05:00)
	// and closes latest (23:00).
	require.Equal(t, 5, grid.Slots[0].Start.In(testutil.IST).Hour())
	require.Equal(t, 23, grid.Slots[len(grid.Slots)-1].End.In(testutil.IST).Hour())

	// The booking we made shows up in the right cell, and only there.
	fi, si := -1, -1
	for i, f := range grid.Facilities {
		if f.ID == testutil.CourtID() {
			fi = i
		}
	}
	for j, s := range grid.Slots {
		if s.Start.Equal(start.UTC()) {
			si = j
		}
	}
	require.GreaterOrEqual(t, fi, 0)
	require.GreaterOrEqual(t, si, 0)
	require.Equal(t, facility.StateBooked, grid.Grid[fi][si])

	// A court that closes at 22:00 reads closed for the 22:00-23:00 column,
	// because a facility that is not open is not free.
	lastCol := len(grid.Slots) - 1
	require.Equal(t, facility.StateClosed, grid.Grid[fi][lastCol],
		"hours outside a facility's own window must not read as bookable")
}

func TestCache_HitAndExpiry(t *testing.T) {
	pg := testutil.Postgres(t)
	rdb := testutil.Redis(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	pool, counter := pg.CountingPool(t)
	require.NoError(t, pool.Ping(ctx))

	av := facility.NewAvailability(pool, rdb, "Asia/Kolkata", nil).WithTTL(1500 * time.Millisecond)

	counter.Reset()
	first, err := av.Campus(ctx, today())
	require.NoError(t, err)
	require.Equal(t, int64(1), counter.Count(), "a cold grid comes from Postgres")

	// Subsequent reads inside the TTL are served from Redis.
	for i := 0; i < 5; i++ {
		_, err := av.Campus(ctx, today())
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), counter.Count(), "cache hits must not query Postgres")

	// A booking made during the window is deliberately NOT visible yet. This is
	// the staleness the design accepts: a stale "free" costs one wasted tap and
	// a fast 409, and can never produce a wrong booking, because the write path
	// never reads this cache.
	start, _ := testutil.Slot18()
	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: testutil.StudentID(0),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	stale, err := av.Campus(ctx, today())
	require.NoError(t, err)
	require.Equal(t, first.Grid, stale.Grid, "within the TTL the grid is the cached one")
	require.Equal(t, int64(1), counter.Count())

	// After the TTL, Postgres is consulted again and the booking appears.
	require.Eventually(t, func() bool {
		g, err := av.Campus(ctx, today())
		if err != nil {
			return false
		}
		for i, f := range g.Facilities {
			if f.ID == testutil.CourtID() {
				for j, s := range g.Slots {
					if s.Start.Equal(start.UTC()) {
						return g.Grid[i][j] == facility.StateBooked
					}
				}
			}
		}
		return false
	}, 6*time.Second, 250*time.Millisecond, "the cache must expire and re-read Postgres")

	require.Greater(t, counter.Count(), int64(1), "expiry must cause a fresh query")
}

// TestCache_MissWhenRedisDown: serving availability from Redis WITHOUT a
// Postgres fallback would turn a cache outage into a feature outage. Here it is
// a latency change nobody notices.
func TestCache_MissWhenRedisDown(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	dead := testutil.DeadRedis()
	t.Cleanup(func() { _ = dead.Close() })

	av := facility.NewAvailability(pg.Pool, dead, "Asia/Kolkata", nil)

	start, _ := testutil.Slot18()
	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: testutil.StudentID(0),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	// Every call falls through to Postgres, and every one is correct — not
	// merely non-erroring.
	for i := 0; i < 3; i++ {
		grid, err := av.Campus(ctx, today())
		require.NoErrorf(t, err, "a Redis outage must not fail the read (call %d)", i)
		require.Len(t, grid.Facilities, 7)

		for fi, f := range grid.Facilities {
			if f.ID != testutil.CourtID() {
				continue
			}
			for si, s := range grid.Slots {
				if s.Start.Equal(start.UTC()) {
					require.Equal(t, facility.StateBooked, grid.Grid[fi][si],
						"the fallback must serve the truth, not an empty grid")
				}
			}
		}
	}
}
