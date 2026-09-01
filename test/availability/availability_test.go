// Package availability_test checks that derived availability agrees with the
// bookings table, always.
//
// This is non-negotiable #4 under test. There is no is_available column, so
// there is nothing to compare a cached flag against — the only way availability
// can be wrong is if the derivation is wrong, and that is what these assert.
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

func newAvailability(t *testing.T, pg *testutil.PG) (*facility.Availability, *facility.Repo) {
	t.Helper()
	repo := facility.NewRepo(pg.Pool)
	return facility.NewAvailability(pg.Pool, nil, "Asia/Kolkata", nil), repo
}

// today is the local date the seeded slot fixtures fall on. Slot fixtures use
// tomorrow so they stay valid for evening runs; derive the query date from the
// same fixture rather than accidentally asking for the real current day.
func today() string {
	return testutil.SlotDate()
}

func slotAt(t *testing.T, day *facility.DayAvailability, start time.Time) facility.Slot {
	t.Helper()
	for _, s := range day.Slots {
		if s.Start.Equal(start.UTC()) {
			return s
		}
	}
	t.Fatalf("no slot starting %s in %d slots (first %s, last %s)",
		start.UTC(), len(day.Slots), day.Slots[0].Start, day.Slots[len(day.Slots)-1].Start)
	return facility.Slot{}
}

func facilityOf(t *testing.T, repo *facility.Repo, id uuid.UUID) *facility.Facility {
	t.Helper()
	f, err := repo.Get(context.Background(), id)
	require.NoError(t, err)
	return f
}

func TestAvailability_FreeSlotsWhenEmpty(t *testing.T) {
	pg := testutil.Postgres(t)
	av, repo := newAvailability(t, pg)
	ctx := context.Background()

	day, err := av.ForFacility(ctx, facilityOf(t, repo, testutil.CourtID()), today())
	require.NoError(t, err)

	// Tennis Court 1 runs 06:00-22:00 IST on a 60 minute grid.
	require.Len(t, day.Slots, 16)
	for _, s := range day.Slots {
		require.Equal(t, facility.StateFree, s.State, "slot %s should be free", s.Start)
		require.Nil(t, s.Remaining, "an exclusive facility has no remaining count")
	}

	require.True(t, day.Slots[0].Start.Before(day.Slots[1].Start), "slots must be ordered")
	require.Equal(t, day.Slots[0].End, day.Slots[1].Start, "and contiguous")
}

func TestAvailability_BookedReflectsConfirmed(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	av, repo := newAvailability(t, pg)
	ctx := context.Background()

	start, _ := testutil.Slot18()
	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: testutil.StudentID(0),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	day, err := av.ForFacility(ctx, facilityOf(t, repo, testutil.CourtID()), today())
	require.NoError(t, err)

	require.Equal(t, facility.StateBooked, slotAt(t, day, start).State)
	require.Equal(t, facility.StateFree, slotAt(t, day, start.Add(time.Hour)).State,
		"only the booked hour is taken")
	require.Equal(t, facility.StateFree, slotAt(t, day, start.Add(-time.Hour)).State)

	// A two-hour booking marks both slots.
	twoStart, _ := testutil.Slot(20, time.Hour)
	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: testutil.StudentID(1),
		Start: twoStart, Duration: 2 * time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	day, err = av.ForFacility(ctx, facilityOf(t, repo, testutil.CourtID()), today())
	require.NoError(t, err)
	require.Equal(t, facility.StateBooked, slotAt(t, day, twoStart).State)
	require.Equal(t, facility.StateBooked, slotAt(t, day, twoStart.Add(time.Hour)).State)
}

func TestAvailability_HeldShownAsHeld(t *testing.T) {
	pg := testutil.Postgres(t)
	av, repo := newAvailability(t, pg)
	ctx := context.Background()

	start, end := testutil.Slot18()

	// A promotion offer. HELD is in the exclusion predicate, so the claim
	// genuinely reserves the court — the grid must say so rather than showing it
	// free to everyone else.
	_, err := pg.Pool.Exec(ctx, `
		INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status, held_until)
		VALUES ($1, $2, true, tstzrange($3::timestamptz, $4::timestamptz, '[)'), 'HELD', now() + interval '10 minutes')`,
		testutil.CourtID(), testutil.StudentID(0), start, end)
	require.NoError(t, err)

	day, err := av.ForFacility(ctx, facilityOf(t, repo, testutil.CourtID()), today())
	require.NoError(t, err)

	require.Equal(t, facility.StateHeld, slotAt(t, day, start).State)
	require.NotEqual(t, facility.StateFree, slotAt(t, day, start).State)
}

func TestAvailability_BlockedShownAsClosed(t *testing.T) {
	pg := testutil.Postgres(t)
	av, repo := newAvailability(t, pg)
	ctx := context.Background()

	start, _ := testutil.Slot(14, time.Hour)
	end := start.Add(3 * time.Hour)

	// A manager closure: user_id NULL, status BLOCKED.
	_, err := pg.Pool.Exec(ctx, `
		INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status)
		VALUES ($1, NULL, true, tstzrange($2::timestamptz, $3::timestamptz, '[)'), 'BLOCKED')`,
		testutil.CourtID(), start, end)
	require.NoError(t, err)

	day, err := av.ForFacility(ctx, facilityOf(t, repo, testutil.CourtID()), today())
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		at := start.Add(time.Duration(i) * time.Hour)
		require.Equalf(t, facility.StateClosed, slotAt(t, day, at).State, "slot %s", at)
	}
	require.Equal(t, facility.StateFree, slotAt(t, day, end).State, "the hour after reopens")
}

func TestAvailability_SharedShowsRemaining(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := pg.BookingService(t)
	av, repo := newAvailability(t, pg)
	ctx := context.Background()

	gym := testutil.GymID()
	start, _ := testutil.Slot18()
	users := pg.Users(t, 30)

	day, err := av.ForFacility(ctx, facilityOf(t, repo, gym), today())
	require.NoError(t, err)

	empty := slotAt(t, day, start)
	require.Equal(t, facility.StateFree, empty.State)
	require.NotNil(t, empty.Remaining)
	require.Equal(t, 30, *empty.Remaining, "an untouched slot has no counter row and is fully free")
	require.Equal(t, 30, *empty.Capacity)

	book := func(n int, from int) {
		t.Helper()
		for i := 0; i < n; i++ {
			_, err := svc.Create(ctx, booking.CreateRequest{
				FacilityID: gym, UserID: users[from+i], Start: start,
				Duration: time.Hour, IdemKey: uuid.NewString(),
			})
			require.NoError(t, err)
		}
	}

	book(10, 0)
	day, _ = av.ForFacility(ctx, facilityOf(t, repo, gym), today())
	s := slotAt(t, day, start)
	require.Equal(t, 20, *s.Remaining)
	require.Equal(t, facility.StateFree, s.State)

	// 20% of 30 is 6, so 24 booked leaves 6 and tips into "filling".
	book(14, 10)
	day, _ = av.ForFacility(ctx, facilityOf(t, repo, gym), today())
	s = slotAt(t, day, start)
	require.Equal(t, 6, *s.Remaining)
	require.Equal(t, facility.StateFilling, s.State)

	book(6, 24)
	day, _ = av.ForFacility(ctx, facilityOf(t, repo, gym), today())
	s = slotAt(t, day, start)
	require.Equal(t, 0, *s.Remaining)
	require.Equal(t, facility.StateFull, s.State)
}

func TestAvailability_RespectsOperatingHours(t *testing.T) {
	pg := testutil.Postgres(t)
	av, repo := newAvailability(t, pg)
	ctx := context.Background()

	cases := []struct {
		facility        uuid.UUID
		opensAt, closes int
		slots           int
	}{
		{testutil.CourtID(), 6, 22, 16},                          // 06:00-22:00
		{testutil.FacilityIDBySlug("football-field"), 6, 20, 14}, // 06:00-20:00
		{testutil.GymID(), 5, 23, 18},                            // 05:00-23:00
	}

	for _, c := range cases {
		f := facilityOf(t, repo, c.facility)
		day, err := av.ForFacility(ctx, f, today())
		require.NoError(t, err)

		require.Lenf(t, day.Slots, c.slots, "%s should expose %d slots", f.Name, c.slots)

		first := day.Slots[0].Start.In(testutil.IST)
		last := day.Slots[len(day.Slots)-1].End.In(testutil.IST)

		require.Equalf(t, c.opensAt, first.Hour(), "%s must start at opens_at", f.Name)
		require.Equalf(t, c.closes, last.Hour(), "%s must end at closes_at", f.Name)

		// Nothing outside the window, in either direction.
		for _, s := range day.Slots {
			h := s.Start.In(testutil.IST).Hour()
			require.GreaterOrEqualf(t, h, c.opensAt, "%s exposed a slot before opening", f.Name)
			require.Lessf(t, h, c.closes, "%s exposed a slot after closing", f.Name)
		}
	}
}

// TestAvailability_TimezoneCorrectAtDayBoundary is the one most likely to catch
// a real bug.
//
// A 23:00 IST slot is 17:30 UTC the same day, but a 00:30 IST slot is 19:00 UTC
// the PREVIOUS day. Anything that resolves "the day" in UTC puts late-evening
// slots on the wrong date, and the symptom is a missing row on the grid rather
// than anything that looks like a timezone mistake.
func TestAvailability_TimezoneCorrectAtDayBoundary(t *testing.T) {
	pg := testutil.Postgres(t)
	av, repo := newAvailability(t, pg)
	ctx := context.Background()

	// The gym closes at 23:00 IST, so its last slot is 22:00-23:00 IST.
	gym := facilityOf(t, repo, testutil.GymID())

	// Pick a fixed date so the assertion does not depend on when it runs.
	const date = "2030-03-15"
	day, err := av.ForFacility(ctx, gym, date)
	require.NoError(t, err)
	require.Len(t, day.Slots, 18)

	last := day.Slots[len(day.Slots)-1]
	lastIST := last.Start.In(testutil.IST)

	require.Equal(t, 22, lastIST.Hour(), "the last slot must start at 22:00 IST")
	require.Equal(t, date, lastIST.Format("2006-01-02"),
		"a 22:00 IST slot belongs to the requested LOCAL date")

	// In UTC that same instant is 16:30 on the same calendar day; the point is
	// that the boundary is computed in IST, so verify the offset explicitly.
	require.Equal(t, 16, last.Start.UTC().Hour())
	require.Equal(t, 30, last.Start.UTC().Minute())

	// The first slot is 05:00 IST, which is 23:30 UTC on the PREVIOUS day. A
	// UTC-based day boundary would drop it from this date entirely.
	first := day.Slots[0]
	require.Equal(t, 5, first.Start.In(testutil.IST).Hour())
	require.Equal(t, date, first.Start.In(testutil.IST).Format("2006-01-02"))
	require.Equal(t, "2030-03-14", first.Start.UTC().Format("2006-01-02"),
		"the earliest IST slot really is on the previous UTC day — which is exactly "+
			"what a UTC-based boundary would get wrong")
}
