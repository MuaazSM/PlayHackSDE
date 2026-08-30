package availability_test

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestAvailabilityMatchesBookings is INV-4 under test.
//
// Availability is derived rather than stored, so the claim "it can never
// disagree with the bookings table" has no column to check — it is a property of
// the derivation. This drives 200 random bookings, cancellations and closures at
// the real write path, then re-derives the ground truth INDEPENDENTLY from the
// bookings table and asserts every single cell agrees.
//
// The independent oracle matters. Re-running the availability query and
// comparing it to itself would prove only that the query is deterministic; this
// compares it to a differently-shaped query over the same rows, so a wrong
// derivation shows up as a disagreement rather than as two matching wrong
// answers.
//
// COVERAGE IS A PROPERTY OF THE TEST RATHER THAN OF THE SEED. An earlier version
// of this test was verifying agreement about a state it never reached: the
// random workload never drove the gym past 24 of its 30 places, so eighteen
// shared slots were checked and every one of them was free, and a wrong
// "filling" threshold passed. Two things prevent that recurring — the run
// asserts that every state actually occurred, and the two boundary states are
// planted deterministically rather than waited for.
func TestAvailabilityMatchesBookings(t *testing.T) {
	const operations = 200

	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	av, repo := newAvailability(t, pg)
	ctx := context.Background()

	// Fixed seed: a property test that fails only on some runs is a rumour. The
	// seed is logged so a failure is reproducible.
	const seed = 20260831
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed=%d operations=%d", seed, operations)

	// Few enough hours that bookings collide. The gym's random hours deliberately
	// exclude 07:00 and 09:00, which carry the planted boundary states.
	hotHours := []int{8, 12, 18, 19}
	gymHours := []int{18, 19}

	exclusive := testutil.ExclusiveFacilityIDs()
	gym := testutil.GymID()
	all := append(append([]uuid.UUID{}, exclusive...), gym)

	users := pg.Users(t, 120)
	var created []uuid.UUID

	// --- Plant the boundary states deterministically -------------------------
	//
	// The random phase below tests AGREEMENT under arbitrary interleaving. It is
	// a poor way to reach a specific state: "filling" is a six-wide band out of
	// thirty, and tuning the generator until it happens to land there produces a
	// test that passes for reasons nobody can restate.
	//
	// So the corners are planted directly, on gym hours the random phase never
	// touches, and the bookings are kept out of the cancellable pool so nothing
	// later perturbs them. Coverage becomes a property of the test rather than
	// of the seed.
	plantGym := func(hour, n int, from int) {
		t.Helper()
		start, _ := testutil.Slot(hour, time.Hour)
		for i := 0; i < n; i++ {
			_, err := svc.Create(ctx, booking.CreateRequest{
				FacilityID: gym, UserID: users[from+i], Start: start,
				Duration: time.Hour, IdemKey: uuid.NewString(),
			})
			require.NoErrorf(t, err, "planting gym booking %d at %02d:00", i, hour)
		}
	}
	plantGym(7, 30, 0)  // exactly capacity  -> full
	plantGym(9, 24, 30) // 6 of 30 remaining -> filling, at the 20% boundary

	var confirmed, conflicted, cancelled, closures int

	for i := 0; i < operations; i++ {
		switch roll := rng.Intn(100); {
		case roll < 65: // book
			// Half the bookings go to the gym, so the shared states are actually
			// reached rather than merely available to be reached.
			f := gym
			if rng.Intn(2) == 0 {
				f = all[rng.Intn(len(all))]
			}
			// Concentrate on a few HOT hours rather than spreading uniformly.
			// A uniform generator produces almost no contention: the earlier
			// version booked the gym so thinly that "filling" and "full" were
			// never reached, so 18 shared slots were checked and every one of
			// them was free. A property test that never reaches a state proves
			// nothing about it.
			hours := hotHours
			if f == gym {
				hours = gymHours
			}
			hour := hours[rng.Intn(len(hours))]
			start, _ := testutil.Slot(hour, time.Hour)

			dur := time.Hour
			// Only exclusive facilities may run long; shared ones are grid-bound
			// to one block by the seed's max_duration.
			if f != gym && rng.Intn(4) == 0 {
				dur = 2 * time.Hour
			}

			b, err := svc.Create(ctx, booking.CreateRequest{
				FacilityID: f, UserID: users[rng.Intn(len(users))],
				Start: start, Duration: dur, IdemKey: uuid.NewString(),
			})
			switch {
			case err == nil:
				confirmed++
				created = append(created, b.ID)
			case errors.Is(err, booking.ErrSlotTaken),
				errors.Is(err, booking.ErrCapacityFull),
				errors.Is(err, booking.ErrValidation):
				conflicted++
			default:
				require.NoErrorf(t, err, "op %d: unexpected error", i)
			}

		case roll < 78 && len(created) > 0: // cancel
			idx := rng.Intn(len(created))
			id := created[idx]
			if _, err := svc.Cancel(ctx, id, testutil.UserIDByRoll("manager01"), "property test"); err == nil {
				cancelled++
			}

		default: // closure
			f := exclusive[rng.Intn(len(exclusive))]
			hour := hotHours[rng.Intn(len(hotHours))]
			start, _ := testutil.Slot(hour, time.Hour)
			_, err := pg.Pool.Exec(ctx, `
				INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status)
				VALUES ($1, NULL, true, tstzrange($2::timestamptz, $3::timestamptz, '[)'), 'BLOCKED')`,
				f, start, start.Add(time.Hour))
			if err == nil {
				closures++
			}
		}
	}

	t.Logf("confirmed=%d conflicted=%d cancelled=%d closures=%d",
		confirmed, conflicted, cancelled, closures)
	require.Positive(t, confirmed, "the run must actually have booked something")
	require.Positive(t, cancelled, "and cancelled something, or the CANCELLED path is untested")
	require.Positive(t, closures)

	// --- Compare every cell against an independent oracle --------------------
	seen := map[string]int{}
	var checked, shared int
	for _, id := range all {
		f := facilityOf(t, repo, id)
		day, err := av.ForFacility(ctx, f, today())
		require.NoError(t, err)

		for _, s := range day.Slots {
			want := oracleState(t, pg, f, s.Start, s.End)
			require.Equalf(t, want, s.State,
				"%s %s: derived availability says %q, the bookings table says %q",
				f.Name, s.Start.In(testutil.IST).Format("15:04"), s.State, want)
			seen[s.State]++
			checked++
			if !f.IsExclusive {
				shared++
			}
		}
	}
	t.Logf("checked %d slots (%d shared) against the bookings table; states seen: %v",
		checked, shared, seen)
	require.Positive(t, shared, "the shared facility must be covered too")

	// COVERAGE, asserted. Without this the run can silently degenerate into
	// checking one state everywhere and still pass — which is exactly how the
	// earlier version missed a wrong "filling" threshold.
	for _, state := range []string{
		facility.StateFree, facility.StateBooked, facility.StateClosed,
		facility.StateFilling, facility.StateFull,
	} {
		require.Positivef(t, seen[state],
			"the generated workload never produced state %q, so nothing was proved "+
				"about it; states seen were %v", state, seen)
	}

	// The campus grid must agree with the per-facility view it summarises.
	grid, err := av.Campus(ctx, today())
	require.NoError(t, err)

	for fi, gf := range grid.Facilities {
		f := facilityOf(t, repo, gf.ID)
		day, err := av.ForFacility(ctx, f, today())
		require.NoError(t, err)

		byStart := map[time.Time]string{}
		for _, s := range day.Slots {
			byStart[s.Start] = s.State
		}

		for si, s := range grid.Slots {
			want, inHours := byStart[s.Start]
			if !inHours {
				// Outside this facility's own hours the grid says closed.
				require.Equalf(t, facility.StateClosed, grid.Grid[fi][si],
					"%s %s is outside opening hours and must read closed",
					gf.Name, s.Start.In(testutil.IST).Format("15:04"))
				continue
			}
			require.Equalf(t, want, grid.Grid[fi][si],
				"campus grid disagrees with the per-facility view for %s at %s",
				gf.Name, s.Start.In(testutil.IST).Format("15:04"))
		}
	}
}

// oracleState recomputes a slot's state directly from the bookings table, in a
// deliberately different shape from the availability query: no generated grid,
// no LATERAL, one targeted question per slot.
func oracleState(t *testing.T, pg *testutil.PG, f *facility.Facility, start, end time.Time) string {
	t.Helper()
	ctx := context.Background()

	var blocked, held, booked int
	require.NoError(t, pg.Pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE status = 'BLOCKED'),
		  count(*) FILTER (WHERE status = 'HELD'),
		  count(*) FILTER (WHERE status = 'CONFIRMED')
		FROM bookings
		 WHERE facility_id = $1
		   AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')`,
		f.ID, start, end).Scan(&blocked, &held, &booked))

	if blocked > 0 {
		return facility.StateClosed
	}

	if f.IsExclusive {
		switch {
		case booked > 0:
			return facility.StateBooked
		case held > 0:
			return facility.StateHeld
		default:
			return facility.StateFree
		}
	}

	// Shared: occupancy is the counter, not the row count.
	var bookedCount, capacity int
	err := pg.Pool.QueryRow(ctx, `
		SELECT booked, capacity FROM slot_capacity
		 WHERE facility_id = $1 AND slot_start = $2`, f.ID, start).Scan(&bookedCount, &capacity)
	if err != nil {
		bookedCount, capacity = 0, f.Capacity
	}

	remaining := capacity - bookedCount
	switch {
	case remaining <= 0:
		return facility.StateFull
	case float64(remaining) <= 0.2*float64(max(capacity, 1)):
		return facility.StateFilling
	default:
		return facility.StateFree
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
