package concurrency_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// writeQueueDepth mirrors WRITE_QUEUE_DEPTH from §2.2, roughly 2.5x the
// PgBouncer backend pool.
const writeQueueDepth = 64

// classify buckets an attempt by the status the HTTP layer will return.
func classify(a testutil.Attempt) string {
	switch {
	case a.Err == nil:
		return "201"
	case errors.Is(a.Err, booking.ErrShed):
		return "429"
	case errors.Is(a.Err, booking.ErrSlotTaken):
		return "409"
	default:
		return "5xx"
	}
}

func report(t *testing.T, label string, out testutil.Outcome) map[string][]testutil.Attempt {
	t.Helper()

	buckets := map[string][]testutil.Attempt{}
	for _, a := range out.Attempts {
		k := classify(a)
		buckets[k] = append(buckets[k], a)
	}

	t.Logf("--- %s (n=%d, elapsed=%s, spread=%s)", label, len(out.Attempts), out.Elapsed, out.StartSpread)
	for _, k := range []string{"201", "409", "429", "5xx"} {
		as := buckets[k]
		if len(as) == 0 {
			continue
		}
		t.Logf("    %s  n=%-4d p50=%-14s p95=%-14s p99=%s",
			k, len(as),
			testutil.Percentile(as, 50),
			testutil.Percentile(as, 95),
			testutil.Percentile(as, 99))
	}
	return buckets
}

// TestShedder_LatencySplit measures the loser path with and without the write
// queue bound, and reports the two rejection classes separately.
//
// The distinction is the point. Meeting p99 < 150ms because conflicts resolve
// quickly is a claim about the write path; meeting it because most of the herd
// was turned away at the door is a claim about admission control. They are not
// the same result and should not be reported as if they were.
func TestShedder_LatencySplit(t *testing.T) {
	const n = 500

	pg := testutil.Postgres(t)
	court := testutil.CourtID()
	start, _ := testutil.Slot18()

	cat := testutil.Catalogue(t, pg)
	testutil.WarmCatalogue(t, cat, court)
	svc := pg.BookingServiceWith(t, cat)
	pg.Warm(t, 25)
	users := pg.Users(t, 2*n)

	// ---- Baseline: no shedder. Every request reaches the database. ---------
	bare := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		return svc.Create(ctx, booking.CreateRequest{
			FacilityID: court, UserID: users[i], Start: start,
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
	})
	bareBuckets := report(t, "no shedder — every request reaches the DB", bare)

	require.Len(t, bareBuckets["201"], 1)
	require.Len(t, bareBuckets["409"], n-1)
	require.Empty(t, bareBuckets["5xx"])

	// ---- With the shedder in front, on a fresh slot. -----------------------
	pg.Reset(t)
	cat.InvalidateAll()
	testutil.WarmCatalogue(t, cat, court)
	users = pg.Users(t, 2*n)

	shedder := httpx.NewShedder(writeQueueDepth, 800*time.Millisecond)

	shed := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		var b *booking.Booking
		err := shedder.Do(ctx, func(ctx context.Context) error {
			var err error
			b, err = svc.Create(ctx, booking.CreateRequest{
				FacilityID: court, UserID: users[i], Start: start,
				Duration: time.Hour, IdemKey: uuid.NewString(),
			})
			return err
		})
		return b, err
	})
	shedBuckets := report(t, "shedder depth=64", shed)

	// The invariant is untouched by admission control.
	var dbCount int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bookings WHERE facility_id = $1 AND status = 'CONFIRMED'`,
		court).Scan(&dbCount))

	require.Empty(t, shedBuckets["5xx"], "shedding must not produce unclassified errors")
	require.Len(t, shedBuckets["201"], 1, "exactly one winner, shedder or not")
	require.Equal(t, 1, dbCount)
	require.NotEmpty(t, shedBuckets["429"], "a burst of %d against depth %d must shed", n, writeQueueDepth)

	// Nothing is lost: every request is a win, a conflict or a shed.
	require.Equal(t, n,
		len(shedBuckets["201"])+len(shedBuckets["409"])+len(shedBuckets["429"]))

	// Admission control must not admit more than the bound allows to matter:
	// the requests that actually reached the database are 201 + 409.
	admitted := len(shedBuckets["201"]) + len(shedBuckets["409"])
	t.Logf("    admitted=%d of %d (%.0f%%), shed=%d",
		admitted, n, 100*float64(admitted)/float64(n), len(shedBuckets["429"]))

	// The claim under test: a shed is cheap.
	shedP99 := testutil.Percentile(shedBuckets["429"], 99)
	require.Less(t, shedP99, 150*time.Millisecond,
		"a 429 must be immediate; p99 was %s", shedP99)
}
