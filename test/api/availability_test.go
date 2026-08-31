package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

func TestHTTP_FacilityAvailability(t *testing.T) {
	a := newAPI(t)
	token := a.login(t, "student01")
	start, _ := testutil.Slot18()

	require.Equal(t, http.StatusCreated,
		a.createBooking(t, token, testutil.CourtID(), start, 60, uuid.NewString()).status)

	date := start.In(testutil.IST).Format("2006-01-02")
	resp := a.do(t, request{
		method: http.MethodGet,
		path:   "/api/v1/facilities/" + testutil.CourtID().String() + "/availability?date=" + date,
		token:  token,
	})
	require.Equal(t, http.StatusOK, resp.status, "body: %s", resp.raw)

	var day struct {
		FacilityID uuid.UUID `json:"facility_id"`
		Date       string    `json:"date"`
		Slots      []struct {
			Start time.Time `json:"start"`
			End   time.Time `json:"end"`
			State string    `json:"state"`
		} `json:"slots"`
	}
	resp.decode(t, &day)

	require.Equal(t, testutil.CourtID(), day.FacilityID)
	require.Equal(t, date, day.Date)
	require.Len(t, day.Slots, 16)

	var booked int
	for _, s := range day.Slots {
		if s.State == "booked" {
			booked++
			require.True(t, s.Start.Equal(start.UTC()))
		}
	}
	require.Equal(t, 1, booked)
}

func TestHTTP_CampusAvailability(t *testing.T) {
	a := newAPI(t)
	token := a.login(t, "student01")

	resp := a.do(t, request{method: http.MethodGet, path: "/api/v1/availability", token: token})
	require.Equal(t, http.StatusOK, resp.status, "body: %s", resp.raw)

	var grid struct {
		Date       string `json:"date"`
		Facilities []struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
		} `json:"facilities"`
		Slots []struct {
			Start time.Time `json:"start"`
		} `json:"slots"`
		Grid [][]string `json:"grid"`
	}
	resp.decode(t, &grid)

	// Defaulting to "today" resolves in IST, not UTC. At 05:00 IST the UTC date
	// is still yesterday, so a UTC default would show the wrong day every
	// morning — and it would look like missing data, not a timezone bug.
	require.Equal(t, time.Now().In(testutil.IST).Format("2006-01-02"), grid.Date)

	require.Len(t, grid.Facilities, 7)
	require.NotEmpty(t, grid.Slots)
	require.Len(t, grid.Grid, len(grid.Facilities))
	for _, row := range grid.Grid {
		require.Len(t, row, len(grid.Slots), "the grid must be a dense rectangle")
	}
}

func TestHTTP_Availability_BadDate(t *testing.T) {
	a := newAPI(t)
	token := a.login(t, "student01")

	for _, bad := range []string{"31-08-2026", "tomorrow", "2026-13-01", "not-a-date"} {
		resp := a.do(t, request{
			method: http.MethodGet, path: "/api/v1/availability?date=" + bad, token: token,
		})
		require.Equalf(t, http.StatusBadRequest, resp.status, "date=%q was accepted", bad)
	}

	// An unknown facility is a 404, not a 500 and not an empty grid.
	resp := a.do(t, request{
		method: http.MethodGet,
		path:   "/api/v1/facilities/" + uuid.NewString() + "/availability",
		token:  token,
	})
	require.Equal(t, http.StatusNotFound, resp.status)
}

// TestHTTP_AvailabilityIsNotShed: only the booking write path is bounded.
//
// Availability is what tells a student who won, and shutting it off during a
// burst is exactly when they need it most. This saturates the write queue and
// reads the grid at the same time — the reads must all succeed while writes are
// being shed.
func TestHTTP_AvailabilityIsNotShed(t *testing.T) {
	a := newAPI(t, withDepth(1))
	token := a.login(t, "student01")
	start, _ := testutil.Slot18()

	const writes, reads = 30, 20

	out := testutil.Race(t, writes+reads, func(ctx context.Context, i int) (any, error) {
		if i < writes {
			return a.createBookingResult(ctx, token, testutil.CourtID(), start, 60, uuid.NewString())
		}
		return a.doResult(ctx, request{method: http.MethodGet, path: "/api/v1/availability", token: token})
	})

	var shedWrites, okReads int
	for _, at := range out.Attempts {
		resp, ok := responseFromAttempt(t, at)
		if !ok {
			continue
		}
		if at.Index < writes {
			if resp.status == http.StatusTooManyRequests {
				shedWrites++
			}
			continue
		}
		require.Equalf(t, http.StatusOK, resp.status,
			"availability request %d was refused while writes were being shed", at.Index)
		okReads++
	}

	require.Equal(t, reads, okReads)
	require.Positive(t, shedWrites, "the write queue must actually have been saturated")
	t.Logf("writes_shed=%d reads_ok=%d", shedWrites, okReads)
}
