// Package analytics_test checks the manager report against a hand-built
// fixture, on a real Postgres.
//
// Every number the endpoint reports is produced by SQL over bookings, waitlist
// and check_ins — there is no rollup table and nothing counts on the write path
// — so the only way to test it is to put known rows in those tables and assert
// the exact arithmetic. A mocked repository would be asserting that the test's
// own fixture equals itself.
//
// The date range is FIXED and in the past. A window anchored on time.Now would
// change shape at midnight IST and would make "expected 0.5" true only some of
// the time.
package analytics_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/analytics"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// The fixture window: Monday 2026-03-02 and Tuesday 2026-03-03, IST.
const (
	day1 = "2026-03-02"
	day2 = "2026-03-03"
	// A window the fixture does not touch, for the date-range test.
	elsewhereFrom = "2026-05-01"
	elsewhereTo   = "2026-05-02"
)

// tz is the campus timezone name the SQL takes as a parameter. Never hardcoded
// inside a query — the server may run anywhere, the student is always in IST.
const tz = "Asia/Kolkata"

type rig struct {
	pg  *testutil.PG
	svc *analytics.Service

	court  uuid.UUID
	court2 uuid.UUID
	gym    uuid.UUID
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pg := testutil.Postgres(t)
	return &rig{
		// No Redis: these tests are about the arithmetic, and a cache in front
		// of it would only let a stale entry answer for a fixture the test just
		// changed. The cache itself is exercised in TestAnalytics_CacheIsNotAuthoritative.
		svc:    analytics.NewService(pg.DB.Replica, nil, tz, nil),
		pg:     pg,
		court:  testutil.CourtID(),
		court2: testutil.Court2ID(),
		gym:    testutil.GymID(),
	}
}

func (r *rig) report(t *testing.T, from, to string) *analytics.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rep, err := r.svc.Report(ctx, from, to)
	require.NoError(t, err)
	return rep
}

// at builds a UTC instant from a campus-local date and hour.
func at(t *testing.T, date string, hour int) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", date)
	require.NoError(t, err)
	return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, testutil.IST).UTC()
}

// booking inserts a row in a chosen status.
//
// Straight SQL rather than the booking service, deliberately: this suite needs
// NO_SHOW, COMPLETED and CANCELLED rows in the past, and driving the service
// through cancel-and-sweep to manufacture them would be testing those paths
// again instead of the reporting query.
func (r *rig) booking(t *testing.T, facility, user uuid.UUID, date string, hour int, hours int, status string) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exclusive := facility != r.gym

	var id uuid.UUID
	err := r.pg.Pool.QueryRow(ctx, `
		INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status)
		VALUES ($1, $2, $3, tstzrange($4, $5, '[)'), $6::booking_status)
		RETURNING id`,
		facility, user, exclusive,
		at(t, date, hour), at(t, date, hour+hours), status,
	).Scan(&id)
	require.NoError(t, err, "insert %s booking", status)
	return id
}

func (r *rig) waitlistEntry(t *testing.T, facility, user uuid.UUID, date string, hour int, status string, bookingID *uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := r.pg.Pool.Exec(ctx, `
		INSERT INTO waitlist (facility_id, user_id, during, status, booking_id)
		VALUES ($1, $2, tstzrange($3, $4, '[)'), $5::waitlist_status, $6)`,
		facility, user, at(t, date, hour), at(t, date, hour+1), status, bookingID,
	)
	require.NoError(t, err, "insert %s waitlist entry", status)
}

func (r *rig) checkIn(t *testing.T, bookingID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := r.pg.Pool.Exec(ctx,
		`INSERT INTO check_ins (booking_id, method) VALUES ($1, 'QR')`, bookingID)
	require.NoError(t, err)
}

// cell pulls one utilisation cell out of the report.
func cell(t *testing.T, rep *analytics.Report, facility uuid.UUID, hour int) analytics.FacilityHour {
	t.Helper()
	for _, c := range rep.Utilisation {
		if c.FacilityID == facility && c.Hour == hour {
			return c
		}
	}
	t.Fatalf("no utilisation cell for facility %s hour %d", facility, hour)
	return analytics.FacilityHour{}
}

func noShowFor(t *testing.T, rep *analytics.Report, facility uuid.UUID) analytics.NoShowRate {
	t.Helper()
	for _, n := range rep.NoShow {
		if n.FacilityID == facility {
			return n
		}
	}
	t.Fatalf("no no-show row for facility %s", facility)
	return analytics.NoShowRate{}
}

// weekdayIndex is the report's matrix row for a campus-local date:
// 0 = Monday .. 6 = Sunday, matching Postgres isodow minus one.
func weekdayIndex(t *testing.T, date string) int {
	t.Helper()
	d, err := time.Parse("2006-01-02", date)
	require.NoError(t, err)
	// time.Weekday is 0 = Sunday; the matrix is 0 = Monday.
	return (int(d.Weekday()) + 6) % 7
}
