package concurrency_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestDepthSweep_Diagnostic measures how the conflict latency responds to
// WRITE_QUEUE_DEPTH. It exists to make the tuning decision evidence-based rather
// than a guess, and to be re-run when the hardware or the schema changes.
//
// It asserts only the shape of the relationship, not absolute numbers — those
// depend on the machine, and an assertion on them would be a flaky test
// pretending to be a benchmark.
//
// Skipped under -short: this is a measurement, not a correctness gate.
func TestDepthSweep_Diagnostic(t *testing.T) {
	if testing.Short() {
		t.Skip("tuning diagnostic; run with -run TestDepthSweep_Diagnostic")
	}

	const n = 500

	pg := testutil.Postgres(t)
	court := testutil.CourtID()
	start, _ := testutil.Slot18()

	type row struct {
		depth       int
		admitted    int
		shed        int
		conflictP99 time.Duration
	}
	var rows []row

	for _, depth := range []int{16, 64, 128, 300, 500} {
		pg.Reset(t)
		cat := testutil.Catalogue(t, pg)
		testutil.WarmCatalogue(t, cat, court)
		svc := pg.BookingServiceWith(t, cat)
		pg.Warm(t, 25)
		users := pg.Users(t, n)

		sh := httpx.NewShedder(depth, 800*time.Millisecond)
		out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
			var b *booking.Booking
			err := sh.Do(ctx, func(ctx context.Context) error {
				var err error
				b, err = svc.Create(ctx, booking.CreateRequest{
					FacilityID: court, UserID: users[i], Start: start,
					Duration: time.Hour, IdemKey: uuid.NewString(),
				})
				return err
			})
			return b, err
		})

		buckets := map[string][]testutil.Attempt{}
		for _, a := range out.Attempts {
			buckets[classify(a)] = append(buckets[classify(a)], a)
		}
		require.Empty(t, buckets["5xx"], "depth=%d produced unclassified errors", depth)

		r := row{
			depth: depth,
			// A 503 is an admitted writer the write timeout retired, so it
			// belongs on the admitted side of the split, not with the shed.
			admitted:    len(buckets["201"]) + len(buckets["409"]) + len(buckets["503"]),
			shed:        len(buckets["429"]),
			conflictP99: testutil.Percentile(buckets["409"], 99),
		}
		rows = append(rows, r)

		t.Logf("depth=%-4d admitted=%-4d shed=%-4d 503=%-3d 409_p99=%-14s 429_p99=%s",
			r.depth, r.admitted, r.shed, len(buckets["503"]), r.conflictP99,
			testutil.Percentile(buckets["429"], 99))
	}

	// The shape: conflict latency rises with the number of requests admitted,
	// because losers serialise behind the per-facility advisory lock. Depth is
	// therefore the knob that trades "how many users get a definitive answer"
	// against "how fast that answer arrives".
	first, last := rows[0], rows[len(rows)-1]
	require.Greater(t, last.conflictP99, first.conflictP99,
		"conflict latency should grow with admitted concurrency; if it does not, "+
			"the bottleneck has moved and this analysis needs redoing")
	require.Less(t, first.shed, n)
}
