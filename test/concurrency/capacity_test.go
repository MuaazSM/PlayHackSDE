package concurrency_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// gymCapacity is the seeded gymnasium's capacity (IMPLEMENTATION.md §0).
const gymCapacity = 30

// attempt is what one goroutine in the take/release race did, so the tally can
// separate a release that decremented from one that matched nothing.
type attempt struct {
	kind    string // "take" | "release"
	applied int    // slots actually decremented by a release
}

func counterFor(t *testing.T, pg *testutil.PG, facilityID uuid.UUID, slotStart time.Time) (booked, capacity int, exists bool) {
	t.Helper()
	err := pg.Pool.QueryRow(context.Background(),
		`SELECT booked, capacity FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
		facilityID, slotStart.UTC()).Scan(&booked, &capacity)
	if err != nil {
		return 0, 0, false
	}
	return booked, capacity, true
}

// TestSharedCapacity_ExactlyC is Mechanism B's equivalent of the single-winner
// race: N concurrent requests against capacity C must yield exactly C
// confirmations, no more and no fewer.
//
// "No fewer" matters as much as "no more". A mechanism that rejected everyone
// would also never overbook, and would be useless.
func TestSharedCapacity_ExactlyC(t *testing.T) {
	const n = 200

	pg := testutil.Postgres(t)
	gym := testutil.GymID()
	start, _ := testutil.Slot18()

	users := pg.Users(t, n)
	cat := testutil.Catalogue(t, pg)
	testutil.WarmCatalogue(t, cat, gym)
	svc := pg.BookingServiceWith(t, cat)
	pg.Warm(t, 20)

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		return svc.Create(ctx, booking.CreateRequest{
			FacilityID: gym, UserID: users[i], Start: start,
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
	})

	confirmed := len(out.Successes())
	full := out.CountIs(booking.ErrCapacityFull)

	var unclassified []error
	for _, a := range out.Failures() {
		if !errors.Is(a.Err, booking.ErrCapacityFull) {
			unclassified = append(unclassified, a.Err)
		}
	}
	require.Empty(t, unclassified, "first unclassified: %v", firstOrNil(unclassified))

	booked, capacity, exists := counterFor(t, pg, gym, start)
	require.True(t, exists)

	var dbConfirmed int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bookings WHERE facility_id = $1 AND status = 'CONFIRMED'`,
		gym).Scan(&dbConfirmed))

	t.Logf("confirmed=%d full=%d booked=%d capacity=%d db_rows=%d %s",
		confirmed, full, booked, capacity, dbConfirmed, out.Summarise())

	require.Equal(t, gymCapacity, confirmed, "exactly capacity may win")
	require.Equal(t, n-gymCapacity, full)
	require.Equal(t, gymCapacity, booked, "the counter must agree with the winners")
	require.Equal(t, gymCapacity, dbConfirmed, "the booking rows must agree with the counter")
	require.Equal(t, gymCapacity, capacity)
}

// TestSharedCapacity_LazyRowCreation: no nightly job pre-materialises the grid,
// so the first booking of a slot creates its counter row.
func TestSharedCapacity_LazyRowCreation(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	gym := testutil.GymID()
	start, _ := testutil.Slot18()

	_, _, exists := counterFor(t, pg, gym, start)
	require.False(t, exists, "no counter row may exist before the first booking")

	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: gym, UserID: testutil.StudentID(0), Start: start,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	booked, capacity, exists := counterFor(t, pg, gym, start)
	require.True(t, exists, "the first booking must create the counter row")
	require.Equal(t, 1, booked)
	require.Equal(t, gymCapacity, capacity)

	// Only for the slot actually booked.
	next, _ := testutil.Slot(19, time.Hour)
	_, _, exists = counterFor(t, pg, gym, next)
	require.False(t, exists, "untouched slots must not be materialised")
}

// TestSharedCapacity_MultiSlot: a two-slot booking increments both counters, in
// one transaction.
func TestSharedCapacity_MultiSlot(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	// The seeded gym is capped at 60-minute bookings, so use a shared facility
	// that allows two blocks.
	hall := pg.Facility(t, "gym", false, 5)
	first, _ := testutil.Slot(18, time.Hour)
	second, _ := testutil.Slot(19, time.Hour)

	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: hall, UserID: testutil.StudentID(0), Start: first,
		Duration: 2 * time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	for _, slot := range []time.Time{first, second} {
		booked, capacity, exists := counterFor(t, pg, hall, slot)
		require.Truef(t, exists, "slot %s must have a counter row", slot)
		require.Equalf(t, 1, booked, "slot %s", slot)
		require.Equal(t, 5, capacity)
	}

	// One booking row spanning both slots, not one row per slot.
	var rows int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE facility_id = $1 AND status = 'CONFIRMED'`,
		hall).Scan(&rows))
	require.Equal(t, 1, rows)
}

// TestSharedCapacity_MultiSlotPartialFails is the atomicity test: if the second
// slot is full, the whole booking rolls back and the first slot must NOT be left
// holding a place nobody has.
func TestSharedCapacity_MultiSlotPartialFails(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	const capacity = 2
	hall := pg.Facility(t, "gym", false, capacity)
	first, _ := testutil.Slot(18, time.Hour)
	second, _ := testutil.Slot(19, time.Hour)

	users := pg.Users(t, 8)

	// Fill the SECOND slot only.
	for i := 0; i < capacity; i++ {
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: hall, UserID: users[i], Start: second,
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
		require.NoError(t, err)
	}

	// One place taken in the first slot, so it has room but is not empty.
	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: hall, UserID: users[2], Start: first,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	beforeFirst, _, _ := counterFor(t, pg, hall, first)
	beforeSecond, _, _ := counterFor(t, pg, hall, second)
	require.Equal(t, 1, beforeFirst)
	require.Equal(t, capacity, beforeSecond)

	// A two-slot booking: room in slot one, none in slot two.
	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: hall, UserID: users[3], Start: first,
		Duration: 2 * time.Hour, IdemKey: uuid.NewString(),
	})
	require.ErrorIs(t, err, booking.ErrCapacityFull)

	afterFirst, _, _ := counterFor(t, pg, hall, first)
	afterSecond, _, _ := counterFor(t, pg, hall, second)

	require.Equal(t, beforeFirst, afterFirst,
		"the first slot was incremented then rolled back; it must not hold a place for a booking that does not exist")
	require.Equal(t, beforeSecond, afterSecond)

	var rows int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE facility_id = $1 AND user_id = $2`,
		hall, users[3]).Scan(&rows))
	require.Equal(t, 0, rows, "no booking row may survive the rollback")
}

// TestCapacityRelease_NoUnderflow: releasing more than was taken is a no-op, and
// the counter never goes negative. A retried cancellation looks exactly like
// this and must be harmless.
func TestCapacityRelease_NoUnderflow(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	gym := testutil.GymID()
	start, end := testutil.Slot18()

	// Release before anything is booked: the counter row does not even exist.
	counters, err := svc.Release(ctx, gym, start, end)
	require.NoError(t, err)
	require.Empty(t, counters, "releasing a slot with no counter row is a no-op")

	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: gym, UserID: testutil.StudentID(0), Start: start,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	booked, _, _ := counterFor(t, pg, gym, start)
	require.Equal(t, 1, booked)

	// First release returns the place.
	counters, err = svc.Release(ctx, gym, start, end)
	require.NoError(t, err)
	require.Len(t, counters, 1)
	require.Equal(t, 0, counters[0].Booked)

	// Every subsequent release is a no-op, never negative.
	for i := 0; i < 5; i++ {
		_, err = svc.Release(ctx, gym, start, end)
		require.NoError(t, err)

		booked, _, _ = counterFor(t, pg, gym, start)
		require.Equal(t, 0, booked, "release %d drove the counter to %d", i+1, booked)
		require.GreaterOrEqual(t, booked, 0)
	}

	// The CHECK (booked >= 0) also holds at the database level.
	var negatives int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM slot_capacity WHERE booked < 0`).Scan(&negatives))
	require.Equal(t, 0, negatives)
}

// TestSlotNotAligned: shared facilities book on a grid, so an off-grid start is
// a 422 and never reaches the counter.
func TestSlotNotAligned(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	gym := testutil.GymID()
	aligned, _ := testutil.Slot18()

	t.Run("off-grid start", func(t *testing.T) {
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: gym, UserID: testutil.StudentID(0),
			Start:    aligned.Add(30 * time.Minute),
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
		require.ErrorIs(t, err, booking.ErrValidation)
		require.Equal(t, booking.CodeSlotNotAligned, booking.Code(err))
		require.Equal(t, "start", booking.Field(err))
	})

	t.Run("partial-block duration", func(t *testing.T) {
		hall := pg.Facility(t, "gym", false, 5)
		start, _ := testutil.Slot(18, time.Hour)
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: hall, UserID: testutil.StudentID(0),
			Start:    start,
			Duration: 90 * time.Minute,
			IdemKey:  uuid.NewString(),
		})
		require.ErrorIs(t, err, booking.ErrValidation)
		require.Equal(t, booking.CodeSlotNotAligned, booking.Code(err))
		require.Equal(t, "duration", booking.Field(err))
	})

	t.Run("exclusive facilities keep variable duration", func(t *testing.T) {
		// The grid must not leak into Mechanism A. An off-grid start on a court
		// is perfectly legal.
		court := testutil.CourtID()
		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: court, UserID: testutil.StudentID(1),
			Start:    aligned.Add(30 * time.Minute),
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
		require.NoError(t, err, "exclusive facilities are not grid-aligned")
	})

	// No counter row was created by any rejected attempt.
	var rows int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM slot_capacity WHERE facility_id = $1`, gym).Scan(&rows))
	require.Equal(t, 0, rows)
}

// TestExclusiveIgnoresCapacityTable: Mechanism A must not touch slot_capacity at
// all. A stray counter row would be a second, disagreeing account of occupancy.
func TestExclusiveIgnoresCapacityTable(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	ctx := context.Background()

	court := testutil.CourtID()
	start, _ := testutil.Slot18()

	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: court, UserID: testutil.StudentID(0), Start: start,
		Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	var rows int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM slot_capacity`).Scan(&rows))
	require.Equal(t, 0, rows, "an exclusive booking must create no capacity rows")
}

// TestConcurrentTakeAndRelease runs 100 takes interleaved with 100 releases on
// one counter row, and checks 0 <= booked <= capacity holds THROUGHOUT — not
// merely at the end, where a counter that had briefly gone negative and come
// back would look perfectly healthy.
//
// The invariant is proved two ways, and the first is the real one:
//
//   - Continuously, by the database. slot_capacity carries CHECK (booked >= 0)
//     and CHECK (booked <= capacity), so any statement that would take the
//     counter out of range fails outright. An excursion cannot happen and then
//     be tidied away; it would surface here as an unclassified error, which the
//     assertions below reject.
//   - By sampling, while the race is in flight, as corroborating evidence that
//     the window really was contended rather than accidentally serialised.
//
// Note that Release is the raw capacity primitive: it returns a place without
// cancelling a booking row, because Phase 4's cancel will do both in one
// transaction. So the counter and the confirmed-row count legitimately diverge
// here, and this test does not assert they agree.
func TestConcurrentTakeAndRelease(t *testing.T) {
	const (
		takes    = 100
		releases = 100
		n        = takes + releases
	)

	pg := testutil.Postgres(t)
	gym := testutil.GymID()
	start, end := testutil.Slot18()

	users := pg.Users(t, takes)
	cat := testutil.Catalogue(t, pg)
	testutil.WarmCatalogue(t, cat, gym)
	svc := pg.BookingServiceWith(t, cat)
	pg.Warm(t, 20)

	// Pre-fill the slot so releases have something real to give back and the two
	// operations genuinely contend, rather than the releases all no-opping on an
	// empty counter.
	seed := pg.Users(t, gymCapacity)
	for i := 0; i < gymCapacity; i++ {
		_, err := svc.Create(context.Background(), booking.CreateRequest{
			FacilityID: gym, UserID: seed[i], Start: start,
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
		require.NoError(t, err)
	}
	initialBooked, _, _ := counterFor(t, pg, gym, start)
	require.Equal(t, gymCapacity, initialBooked)

	// Sample the counter while the race runs.
	var (
		samples    atomic.Int64
		violations atomic.Int64
		minSeen    atomic.Int64
		maxSeen    atomic.Int64
	)
	minSeen.Store(int64(initialBooked))
	maxSeen.Store(int64(initialBooked))

	stopSampling := make(chan struct{})
	samplingDone := make(chan struct{})
	go func() {
		defer close(samplingDone)
		for {
			select {
			case <-stopSampling:
				return
			default:
			}

			var b, c int
			err := pg.Pool.QueryRow(context.Background(),
				`SELECT booked, capacity FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
				gym, start.UTC()).Scan(&b, &c)
			if err == nil {
				samples.Add(1)
				if b < 0 || b > c {
					violations.Add(1)
				}
				for {
					old := minSeen.Load()
					if int64(b) >= old || minSeen.CompareAndSwap(old, int64(b)) {
						break
					}
				}
				for {
					old := maxSeen.Load()
					if int64(b) <= old || maxSeen.CompareAndSwap(old, int64(b)) {
						break
					}
				}
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		// Even indices take, odd indices release: 100 of each, interleaved on
		// the same counter row.
		if i%2 == 1 {
			counters, err := svc.Release(ctx, gym, start, end)
			// applied counts slots that ACTUALLY decremented. A release that
			// matched zero rows is a no-op and returns no counter — tallying it
			// as a decrement is exactly the lost-update the ledger below exists
			// to catch.
			return attempt{kind: "release", applied: len(counters)}, err
		}

		_, err := svc.Create(ctx, booking.CreateRequest{
			FacilityID: gym, UserID: users[i/2], Start: start,
			Duration: time.Hour, IdemKey: uuid.NewString(),
		})
		return attempt{kind: "take"}, err
	})

	close(stopSampling)
	<-samplingDone

	// Releases never fail. Takes either succeed or find the slot full; anything
	// else — including a CHECK violation from an out-of-range counter — is a
	// defect.
	var tookOK, tookFull, releaseCalls, releasedOK, releaseNoop int
	for _, a := range out.Attempts {
		r, _ := a.Value.(attempt)
		switch {
		case a.Err == nil && r.kind == "release":
			releaseCalls++
			releasedOK += r.applied
			if r.applied == 0 {
				releaseNoop++
			}
		case a.Err == nil:
			tookOK++
		case errors.Is(a.Err, booking.ErrCapacityFull) && r.kind == "take":
			tookFull++
		default:
			t.Fatalf("attempt %d (%s) failed with an unclassified error, which is how a "+
				"CHECK violation on the counter would present: %v", a.Index, r.kind, a.Err)
		}
	}
	require.Equal(t, releases, releaseCalls, "a release must never fail")
	require.Equal(t, takes, tookOK+tookFull, "every take is a confirmation or a full slot")

	finalBooked, finalCapacity, _ := counterFor(t, pg, gym, start)

	t.Logf("takes=%d (ok=%d full=%d) releases=%d (applied=%d noop=%d) initial=%d final_booked=%d capacity=%d",
		takes, tookOK, tookFull, releases, releasedOK, releaseNoop,
		initialBooked, finalBooked, finalCapacity)
	t.Logf("sampled=%d violations=%d observed_range=[%d,%d] %s",
		samples.Load(), violations.Load(), minSeen.Load(), maxSeen.Load(), out.Summarise())

	require.Zero(t, violations.Load(),
		"sampled the counter %d times and saw %d readings outside [0,%d]",
		samples.Load(), violations.Load(), finalCapacity)

	// Conservation. A range check passes even when an update is lost: a release
	// tallied as applied that matched zero rows, or a decrement applied twice
	// but landing back inside [0, capacity], are both invisible to it. The
	// counter must equal the ledger of what actually happened to it.
	require.Equal(t, initialBooked+tookOK-releasedOK, finalBooked,
		"counter drifted from the take/release ledger")

	require.GreaterOrEqual(t, finalBooked, 0)
	require.LessOrEqual(t, finalBooked, finalCapacity)
	require.GreaterOrEqual(t, minSeen.Load(), int64(0), "the counter went negative mid-race")
	require.LessOrEqual(t, maxSeen.Load(), int64(finalCapacity), "the counter exceeded capacity mid-race")

	// The database's own guarantee, restated: no row anywhere is out of range.
	var bad int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM slot_capacity WHERE booked < 0 OR booked > capacity`).Scan(&bad))
	require.Equal(t, 0, bad, "the CHECK constraints must hold throughout")

	// The race must have actually interleaved: with 100 releases against a slot
	// pre-filled to capacity, the counter has to have dropped below its start.
	require.Less(t, minSeen.Load(), int64(gymCapacity),
		"no release was observed taking effect; takes and releases did not interleave")
}

// TestSharedCapacity_ConcurrentMultiSlot contends on overlapping slot SETS, not
// just one slot.
//
// This is the case the ascending-order rule exists for: every caller claims its
// counter rows in ascending slot_start, so two transactions wanting the same
// pair of rows acquire them in the same order and cannot wait on each other.
// Inconsistent ordering would show up here as deadlocks — unclassified errors
// rather than clean full-slot rejections.
func TestSharedCapacity_ConcurrentMultiSlot(t *testing.T) {
	const (
		n        = 120
		capacity = 10
	)

	pg := testutil.Postgres(t)
	hall := pg.Facility(t, "gym", false, capacity)

	first, _ := testutil.Slot(18, time.Hour)
	second, _ := testutil.Slot(19, time.Hour)

	users := pg.Users(t, n)
	cat := testutil.Catalogue(t, pg)
	testutil.WarmCatalogue(t, cat, hall)
	svc := pg.BookingServiceWith(t, cat)
	pg.Warm(t, 20)

	// Half book 18:00-20:00 (both slots), half book 19:00-20:00 (the second
	// only), so the two groups contend on an overlapping but unequal set.
	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		req := booking.CreateRequest{
			FacilityID: hall, UserID: users[i], Start: first,
			Duration: 2 * time.Hour, IdemKey: uuid.NewString(),
		}
		if i%2 == 1 {
			req.Start, req.Duration = second, time.Hour
		}
		return svc.Create(ctx, req)
	})

	for _, a := range out.Failures() {
		require.ErrorIsf(t, a.Err, booking.ErrCapacityFull,
			"attempt %d failed with something other than a full slot — a deadlock here "+
				"means slots were not claimed in a consistent order: %v", a.Index, a.Err)
	}

	firstBooked, _, _ := counterFor(t, pg, hall, first)
	secondBooked, _, _ := counterFor(t, pg, hall, second)

	var confirmed int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bookings WHERE facility_id = $1 AND status = 'CONFIRMED'`,
		hall).Scan(&confirmed))

	t.Logf("confirmed=%d full=%d slot1_booked=%d slot2_booked=%d %s",
		len(out.Successes()), out.CountIs(booking.ErrCapacityFull),
		firstBooked, secondBooked, out.Summarise())

	// The second slot is claimed by both groups, so it is the binding
	// constraint and must land exactly at capacity.
	require.Equal(t, capacity, secondBooked, "the contended slot must fill exactly to capacity")
	require.LessOrEqual(t, firstBooked, capacity)

	// Every confirmed booking is accounted for in the counters: two-slot
	// bookings touch both rows, one-slot bookings touch only the second.
	require.Equal(t, confirmed, secondBooked)
	require.Equal(t, len(out.Successes()), confirmed)
}
