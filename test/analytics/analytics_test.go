package analytics_test

import (
	"context"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/analytics"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestAnalytics_UtilisationMath is the one that has to be exact.
//
// Tennis Court 1 is exclusive, capacity 1, open 06:00–22:00 IST, so over the
// two-day window each hour-of-day cell offers 2 available hours. The gymnasium
// is shared with capacity 30 and open 05:00–23:00, so each of its cells offers
// 2 x 30 = 60 person-hours. Dividing the gym by 2 instead of 60 would report
// three people in a thirty-person room as 150% full, which is why capacity is
// in the denominator and why this test checks both kinds of facility.
//
// The expected values below are computed by hand from the fixture, not read
// back from the query. A test that asks the query what the answer is and then
// asserts it proves only that the query is deterministic.
func TestAnalytics_UtilisationMath(t *testing.T) {
	r := newRig(t)
	students := testutil.StudentIDs()

	// Court 1: one 2-hour booking on day 1, 18:00–20:00 IST.
	r.booking(t, r.court, students[0], day1, 18, 2, "CONFIRMED")

	// Gym: three separate one-hour bookings on day 1, 07:00–08:00 IST.
	// Mechanism B's facility, so overlapping rows are legal and expected.
	for i := 0; i < 3; i++ {
		r.booking(t, r.gym, students[i], day1, 7, 1, "CONFIRMED")
	}

	rep := r.report(t, day1, day2)

	// Court 1, 18:00 — one booked hour out of two available.
	c := cell(t, rep, r.court, 18)
	require.Equal(t, 2.0, c.AvailableHours, "2 days x 1 hour x capacity 1")
	require.Equal(t, 1.0, c.BookedHours)
	require.InDelta(t, 0.5, c.Utilisation, 1e-9)

	// The second hour of the same booking lands in the 19:00 cell. A query that
	// bucketed by start time alone would report this one as empty.
	c = cell(t, rep, r.court, 19)
	require.Equal(t, 1.0, c.BookedHours, "the second hour of a 2-hour booking")
	require.InDelta(t, 0.5, c.Utilisation, 1e-9)

	// 20:00 is open and untouched: zero, and present.
	c = cell(t, rep, r.court, 20)
	require.Equal(t, 2.0, c.AvailableHours)
	require.Equal(t, 0.0, c.BookedHours)
	require.Equal(t, 0.0, c.Utilisation)

	// Gym, 07:00 — three person-hours out of sixty.
	c = cell(t, rep, r.gym, 7)
	require.Equal(t, 60.0, c.AvailableHours, "2 days x 1 hour x capacity 30")
	require.Equal(t, 3.0, c.BookedHours)
	require.InDelta(t, 0.05, c.Utilisation, 1e-9)

	// Hours outside opening time are not supply and must not appear at all.
	// Reporting a closed hour as 0% utilised would drag every daily average
	// down with time the facility never offered.
	for _, c := range rep.Utilisation {
		if c.FacilityID == r.court {
			require.GreaterOrEqual(t, c.Hour, 6, "court 1 opens at 06:00")
			require.Less(t, c.Hour, 22, "court 1 closes at 22:00")
		}
	}
}

// TestAnalytics_NoShowRate feeds M-6.
//
// The denominator is every booking that reached its slot. A cancellation is the
// behaviour the system WANTS — it returns the court in time for the waitlist —
// so it must not sit in the denominator diluting the rate, and certainly not in
// the numerator punishing it.
func TestAnalytics_NoShowRate(t *testing.T) {
	r := newRig(t)
	s := testutil.StudentIDs()

	r.booking(t, r.court, s[0], day1, 8, 1, "COMPLETED")
	r.booking(t, r.court, s[1], day1, 9, 1, "NO_SHOW")
	r.booking(t, r.court, s[2], day1, 10, 1, "CONFIRMED")
	// Two cancellations: invisible to both halves of the fraction.
	r.booking(t, r.court, s[3], day1, 11, 1, "CANCELLED")
	r.booking(t, r.court, s[4], day1, 12, 1, "CANCELLED")

	rep := r.report(t, day1, day2)

	n := noShowFor(t, rep, r.court)
	require.Equal(t, 3, n.Total, "COMPLETED + NO_SHOW + CONFIRMED; cancellations excluded")
	require.Equal(t, 1, n.NoShows)
	require.InDelta(t, 1.0/3.0, n.Rate, 1e-9)

	// A facility nobody booked reports 0, not a missing row and not a division
	// by zero.
	idle := noShowFor(t, rep, r.court2)
	require.Equal(t, 0, idle.Total)
	require.Equal(t, 0, idle.NoShows)
	require.Equal(t, 0.0, idle.Rate)
}

// TestAnalytics_PeakHeatmapShape pins the dense 7x24 contract.
//
// The client renders this as a table and indexes cells[weekday][hour] without
// bounds checks. A ragged matrix, or one that omits quiet days, would either
// panic the console or silently shift every column.
func TestAnalytics_PeakHeatmapShape(t *testing.T) {
	r := newRig(t)
	s := testutil.StudentIDs()

	// Two bookings and one waitlist entry, all at 18:00 on day 1. Demand is
	// bookings PLUS the queue: an hour with one booking and four people waiting
	// is not as busy as an hour with one booking, and utilisation alone cannot
	// say so because it saturates at 1.0.
	r.booking(t, r.court, s[0], day1, 18, 1, "CONFIRMED")
	r.booking(t, r.court2, s[1], day1, 18, 1, "CONFIRMED")
	r.waitlistEntry(t, r.court, s[2], day1, 18, "WAITING", nil)

	rep := r.report(t, day1, day2)
	h := rep.PeakDemand

	require.Len(t, h.Weekdays, 7)
	require.Len(t, h.Hours, 24)
	require.Len(t, h.Cells, 7, "one row per weekday, including quiet ones")
	for d, row := range h.Cells {
		require.Len(t, row, 24, "weekday %d must have all 24 hours", d)
	}

	monday := weekdayIndex(t, day1)
	require.Equal(t, 3, h.Cells[monday][18], "2 bookings + 1 waitlist entry")
	require.Equal(t, 0, h.Cells[monday][19], "a quiet hour is a zero, not a gap")

	require.Equal(t, monday, h.Peak.Weekday)
	require.Equal(t, 18, h.Peak.Hour)
	require.Equal(t, 3, h.Peak.Count)
}

// TestAnalytics_UnmetDemandFromWaitlist is the number utilisation cannot show.
//
// Two courts at 100% look identical on the occupancy chart; the one with four
// students queued behind it is the one that justifies a new court.
func TestAnalytics_UnmetDemandFromWaitlist(t *testing.T) {
	r := newRig(t)
	s := testutil.StudentIDs()

	for i := 0; i < 4; i++ {
		r.waitlistEntry(t, r.court, s[i], day1, 18, "WAITING", nil)
	}
	// Somebody who left the queue is not unmet demand.
	r.waitlistEntry(t, r.court, s[4], day1, 18, "CANCELLED", nil)
	// A different hour, so the bucketing is actually doing something.
	r.waitlistEntry(t, r.court, s[5], day1, 19, "EXPIRED", nil)

	rep := r.report(t, day1, day2)

	depth := map[int]int{}
	for _, u := range rep.UnmetDemand {
		if u.FacilityID == r.court {
			depth[u.Hour] = u.Entries
		}
	}
	require.Equal(t, 4, depth[18], "CANCELLED entries do not count as unmet demand")
	require.Equal(t, 1, depth[19], "an expired offer was still somebody who wanted the hour")
	require.NotContains(t, depth, 20)
}

// TestAnalytics_SlotRecoveryRate feeds M-7.
//
// The numerator is ATTENDANCE, not promotion and not even claiming. A
// cancellation that promotes somebody who then never turns up recovered
// nothing — the slot was wasted twice — and a metric that counted it as a
// success would make the waitlist look best exactly when it is failing.
func TestAnalytics_SlotRecoveryRate(t *testing.T) {
	r := newRig(t)
	s := testutil.StudentIDs()

	// Four offers. One checked in, one was swept to COMPLETED, one no-showed,
	// one let the offer expire.
	attended := r.booking(t, r.court, s[0], day1, 14, 1, "CONFIRMED")
	r.checkIn(t, attended)
	r.waitlistEntry(t, r.court, s[0], day1, 14, "CLAIMED", &attended)

	completed := r.booking(t, r.court, s[1], day1, 15, 1, "COMPLETED")
	r.waitlistEntry(t, r.court, s[1], day1, 15, "CLAIMED", &completed)

	noShow := r.booking(t, r.court, s[2], day1, 16, 1, "NO_SHOW")
	r.waitlistEntry(t, r.court, s[2], day1, 16, "CLAIMED", &noShow)

	expired := r.booking(t, r.court, s[3], day1, 17, 1, "CANCELLED")
	r.waitlistEntry(t, r.court, s[3], day1, 17, "EXPIRED", &expired)

	// Still WAITING, never offered anything: outside the fraction entirely.
	r.waitlistEntry(t, r.court, s[4], day1, 18, "WAITING", nil)

	rep := r.report(t, day1, day2)

	require.Equal(t, 4, rep.SlotRecovery.Promoted, "WAITING entries were never offered a slot")
	require.Equal(t, 2, rep.SlotRecovery.Recovered, "checked in, or swept to COMPLETED")
	require.InDelta(t, 0.5, rep.SlotRecovery.Rate, 1e-9)
}

// TestAnalytics_RespectsDateRange: a window the fixture does not touch must
// report zeroes, not the whole table.
func TestAnalytics_RespectsDateRange(t *testing.T) {
	r := newRig(t)
	s := testutil.StudentIDs()

	r.booking(t, r.court, s[0], day1, 18, 1, "CONFIRMED")
	r.booking(t, r.court, s[1], day1, 9, 1, "NO_SHOW")
	r.waitlistEntry(t, r.court, s[2], day1, 18, "WAITING", nil)

	// In range: the fixture is visible.
	inRange := r.report(t, day1, day2)
	require.Equal(t, 1.0, cell(t, inRange, r.court, 18).BookedHours)
	require.Equal(t, 1, noShowFor(t, inRange, r.court).NoShows)
	require.NotEmpty(t, inRange.UnmetDemand)

	// Out of range: same database, nothing to report.
	out := r.report(t, elsewhereFrom, elsewhereTo)
	require.Equal(t, 0.0, cell(t, out, r.court, 18).BookedHours)
	require.Equal(t, 0.0, cell(t, out, r.court, 18).Utilisation)
	require.Equal(t, 2.0, cell(t, out, r.court, 18).AvailableHours,
		"supply still exists in the window; only the demand is absent")
	require.Equal(t, 0, noShowFor(t, out, r.court).Total)
	require.Empty(t, out.UnmetDemand)
	require.Equal(t, 0, out.SlotRecovery.Promoted)

	// A single-day window is inclusive at both ends, so day 1 alone still sees
	// the day-1 booking. An exclusive upper bound here would silently drop
	// whatever the manager asked about.
	oneDay := r.report(t, day1, day1)
	require.Equal(t, 1.0, cell(t, oneDay, r.court, 18).BookedHours)
	require.Equal(t, 1.0, cell(t, oneDay, r.court, 18).AvailableHours, "one day of supply")
	require.Equal(t, 1.0, cell(t, oneDay, r.court, 18).Utilisation)
}

// TestAnalytics_ExcludesCancelled: a cancelled booking occupied nothing, and a
// report that counted it would show courts as busy during hours that were in
// fact free — the exact opposite of what the manager needs to plan.
func TestAnalytics_ExcludesCancelled(t *testing.T) {
	r := newRig(t)
	s := testutil.StudentIDs()

	// Same court, same hour: booked then cancelled, then booked again. Legal
	// because CANCELLED is outside no_double_book's predicate.
	r.booking(t, r.court, s[0], day1, 18, 1, "CANCELLED")
	r.booking(t, r.court, s[1], day1, 18, 1, "CONFIRMED")
	// A cancelled-only hour.
	r.booking(t, r.court, s[2], day1, 13, 1, "CANCELLED")

	rep := r.report(t, day1, day2)

	require.Equal(t, 1.0, cell(t, rep, r.court, 18).BookedHours,
		"the cancellation must not double-count the hour")
	require.Equal(t, 0.0, cell(t, rep, r.court, 13).BookedHours)
	require.Equal(t, 0.0, cell(t, rep, r.court, 13).Utilisation)

	require.Equal(t, 1, noShowFor(t, rep, r.court).Total, "only the CONFIRMED row reached its slot")

	// And a cancellation is not demand on the heatmap either — it is a request
	// that was withdrawn.
	monday := weekdayIndex(t, day1)
	require.Equal(t, 1, rep.PeakDemand.Cells[monday][18])
	require.Equal(t, 0, rep.PeakDemand.Cells[monday][13])
}

// TestAnalytics_EmptyRangeReturnsZeroesNotNulls.
//
// The console renders numbers. A null utilisation, a nil slice or an absent
// facility all end up as "NaN%" or a blank cell on the screen, and a blank cell
// is indistinguishable from a loading failure. Empty means zero here, and zero
// is a fact.
func TestAnalytics_EmptyRangeReturnsZeroesNotNulls(t *testing.T) {
	r := newRig(t)

	rep := r.report(t, elsewhereFrom, elsewhereTo)

	require.Equal(t, elsewhereFrom, rep.From)
	require.Equal(t, elsewhereTo, rep.To)

	require.NotNil(t, rep.Utilisation)
	require.NotEmpty(t, rep.Utilisation, "supply exists even when nothing was booked")
	for _, c := range rep.Utilisation {
		require.Equal(t, 0.0, c.BookedHours)
		require.Equal(t, 0.0, c.Utilisation, "no division-by-zero NaN, no null")
		require.Greater(t, c.AvailableHours, 0.0)
	}

	require.NotNil(t, rep.NoShow)
	require.Len(t, rep.NoShow, 7, "every active seeded facility, including the idle ones")
	for _, n := range rep.NoShow {
		require.Equal(t, 0, n.Total)
		require.Equal(t, 0, n.NoShows)
		require.Equal(t, 0.0, n.Rate)
	}

	require.NotNil(t, rep.UnmetDemand, "an empty list, never null")
	require.Empty(t, rep.UnmetDemand)

	require.Len(t, rep.PeakDemand.Cells, 7)
	for _, row := range rep.PeakDemand.Cells {
		require.Len(t, row, 24)
		for _, v := range row {
			require.Equal(t, 0, v)
		}
	}
	require.Equal(t, 0, rep.PeakDemand.Peak.Count)

	require.Equal(t, analytics.SlotRecovery{Promoted: 0, Recovered: 0, Rate: 0}, rep.SlotRecovery)
}

// TestAnalytics_RejectsAbsurdRange.
//
// The bound is a footgun guard, not a performance tweak: the utilisation query
// materialises one cell per facility per open hour per day, so a century-wide
// window is a request to build tens of millions of rows on the read replica the
// student-facing availability path shares.
func TestAnalytics_RejectsAbsurdRange(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	for _, tc := range []struct{ name, from, to string }{
		{"backwards", day2, day1},
		{"unparseable from", "yesterday", day2},
		{"unparseable to", day1, "31-12-2026"},
		{"a century", "2000-01-01", "2100-01-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.svc.Report(ctx, tc.from, tc.to)
			require.ErrorIs(t, err, analytics.ErrBadRange)
		})
	}
}

// TestAnalytics_CacheIsNotAuthoritative.
//
// Non-negotiable #3, applied to the one place in this feature that touches
// Redis. The cache may serve a stale report — 60s of staleness on a month's
// utilisation is nothing anybody acts on differently — but wiping Redis must
// only cost a query, never an empty console or a wrong number.
func TestAnalytics_CacheIsNotAuthoritative(t *testing.T) {
	pg := testutil.Postgres(t)
	rdb := testutil.Redis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, rdb.FlushAll(ctx).Err())

	r := &rig{pg: pg, court: testutil.CourtID(), court2: testutil.Court2ID(), gym: testutil.GymID()}
	r.svc = analytics.NewService(pg.DB.Replica, rdb, tz, nil)

	s := testutil.StudentIDs()
	r.booking(t, r.court, s[0], day1, 18, 1, "CONFIRMED")

	first := r.report(t, day1, day2)
	require.Equal(t, 1.0, cell(t, first, r.court, 18).BookedHours)

	// Warm entry: a second booking is invisible until the TTL expires. That is
	// the trade being made, stated out loud.
	r.booking(t, r.court, s[1], day1, 19, 1, "CONFIRMED")
	cached := r.report(t, day1, day2)
	require.Equal(t, 0.0, cell(t, cached, r.court, 19).BookedHours,
		"a warm entry is served as-is; 60s of staleness on a report is the deal")

	// Wipe Redis mid-run. The next report is the truth, straight from Postgres.
	require.NoError(t, rdb.FlushAll(ctx).Err())
	fresh := r.report(t, day1, day2)
	require.Equal(t, 1.0, cell(t, fresh, r.court, 19).BookedHours,
		"losing the cache costs a query, not correctness")

	// And a Redis that is simply gone is not an error the manager sees.
	dead := analytics.NewService(pg.DB.Replica, testutil.DeadRedis(), tz, nil)
	rep, err := dead.Report(ctx, day1, day2)
	require.NoError(t, err)
	require.Equal(t, 1.0, cell(t, rep, r.court, 19).BookedHours)
}
