// Package demo_test exercises the race console against a real Postgres.
//
// The race console is the single most protected deliverable in this project
// (CLAUDE.md, demo priorities #1): if it breaks, the 45 seconds the whole build
// is aimed at do not happen. So it gets the same treatment as the write path —
// a real database, real contention, and assertions on the database's own answer
// rather than on what the process running the race believes.
package demo_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/demo"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// harness is a demo service over a freshly reset, freshly seeded database.
type harness struct {
	pg    *testutil.PG
	svc   *demo.Service
	court uuid.UUID
	gym   uuid.UUID
	start time.Time
	end   time.Time
}

// newHarness wires the race console exactly as the API and the CLI do: over the
// same booking service a student books through.
func newHarness(t *testing.T) *harness {
	t.Helper()

	pg := testutil.Postgres(t)
	cat := testutil.Catalogue(t, pg)
	bookings := pg.BookingServiceWith(t, cat)

	court := testutil.CourtID()
	gym := testutil.GymID()
	testutil.WarmCatalogue(t, cat, court, gym)
	pg.Warm(t, 20)

	start, end := futureSlot()

	return &harness{
		pg:    pg,
		svc:   demo.NewService(pg.DB, bookings, cat),
		court: court,
		gym:   gym,
		start: start,
		end:   end,
	}
}

// futureSlot is tomorrow at 18:00 IST for one hour, in UTC.
//
// Tomorrow rather than today on purpose: the write path rejects a start in the
// past, so a suite pinned to today's 18:00 would pass all morning and fail every
// evening. A test whose result depends on what time it is run is worse than no
// test — it teaches the team to ignore a red build.
func futureSlot() (start, end time.Time) {
	now := time.Now().In(testutil.IST)
	start = time.Date(now.Year(), now.Month(), now.Day(), 18, 0, 0, 0, testutil.IST).
		AddDate(0, 0, 1).UTC()
	return start, start.Add(time.Hour)
}

func (h *harness) ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// confirmedInSlot counts confirmed bookings for the demo slot with a query
// written out here rather than reused from the service.
//
// Deliberate duplication: if the test asked the same helper the code under test
// asks, a helper that counted the wrong thing would agree with itself and the
// suite would prove nothing. This is the independent second opinion.
func (h *harness) confirmedInSlot(t *testing.T, facilityID uuid.UUID) int {
	t.Helper()

	var n int
	require.NoError(t, h.pg.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM bookings
		 WHERE facility_id = $1
		   AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')
		   AND status = 'CONFIRMED'`,
		facilityID, h.start, h.end).Scan(&n))
	return n
}

func (h *harness) race(t *testing.T, n int) *demo.Result {
	t.Helper()

	res, err := h.svc.Run(h.ctx(t), demo.Request{
		FacilityID: h.court,
		Start:      h.start,
		Duration:   time.Hour,
		N:          n,
	})
	require.NoError(t, err)
	return res
}

func (h *harness) reset(t *testing.T) *demo.ResetResult {
	t.Helper()

	res, err := h.svc.Reset(h.ctx(t), h.court, h.start, h.end)
	require.NoError(t, err)
	return res
}
