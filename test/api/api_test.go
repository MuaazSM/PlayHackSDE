package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

func TestHTTP_CreateBooking_201(t *testing.T) {
	a := newAPI(t)
	token := a.login(t, "student01")
	start, _ := testutil.Slot18()

	resp := a.createBooking(t, token, testutil.CourtID(), start, 60, uuid.NewString())
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.raw)

	var b struct {
		ID         uuid.UUID `json:"id"`
		Reference  string    `json:"reference"`
		FacilityID uuid.UUID `json:"facility_id"`
		Start      time.Time `json:"start"`
		End        time.Time `json:"end"`
		Status     string    `json:"status"`
	}
	resp.decode(t, &b)

	require.NotEqual(t, uuid.Nil, b.ID)
	require.Equal(t, testutil.CourtID(), b.FacilityID)
	require.Equal(t, "CONFIRMED", b.Status)
	require.True(t, b.Start.Equal(start))
	require.True(t, b.End.Equal(start.Add(time.Hour)))
	require.Contains(t, b.Reference, "PH-")

	// The row is really there.
	var n int
	require.NoError(t, a.pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bookings WHERE id = $1 AND status = 'CONFIRMED'`, b.ID).Scan(&n))
	require.Equal(t, 1, n)
}

func TestHTTP_Replay_200SameBooking(t *testing.T) {
	a := newAPI(t)
	token := a.login(t, "student01")
	start, _ := testutil.Slot18()
	key := uuid.NewString()

	first := a.createBooking(t, token, testutil.CourtID(), start, 60, key)
	require.Equal(t, http.StatusCreated, first.status)

	second := a.createBooking(t, token, testutil.CourtID(), start, 60, key)
	require.Equal(t, http.StatusOK, second.status,
		"a replay is a success the client already has, not a conflict: %s", second.raw)

	var a1, a2 struct {
		ID        uuid.UUID `json:"id"`
		Reference string    `json:"reference"`
	}
	first.decode(t, &a1)
	second.decode(t, &a2)

	require.Equal(t, a1.ID, a2.ID, "the replay must carry the ORIGINAL booking")
	require.Equal(t, a1.Reference, a2.Reference)

	var n int
	require.NoError(t, a.pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bookings WHERE facility_id = $1 AND status = 'CONFIRMED'`,
		testutil.CourtID()).Scan(&n))
	require.Equal(t, 1, n, "a replay must not create a second row")
}

func TestHTTP_Conflict_409WithErrorCode(t *testing.T) {
	a := newAPI(t)
	start, _ := testutil.Slot18()

	winner := a.login(t, "student01")
	require.Equal(t, http.StatusCreated,
		a.createBooking(t, winner, testutil.CourtID(), start, 60, uuid.NewString()).status)

	loser := a.login(t, "student02")
	resp := a.createBooking(t, loser, testutil.CourtID(), start, 60, uuid.NewString())
	require.Equal(t, http.StatusConflict, resp.status)

	body := resp.errorBody(t)
	require.Equal(t, httpx.CodeSlotTaken, body.Error, "the client switches on this code")
	require.NotEmpty(t, body.Message, "and shows this to the user")
	require.NotEqual(t, body.Error, body.Message, "code and prose are different fields")
	require.NotEmpty(t, body.RequestID, "every envelope carries a request id")

	// Alternatives arrive in Phase 7; the field is absent rather than empty so
	// the shape does not change under clients when it lands.
	require.Empty(t, body.Alternatives)
}

func TestHTTP_MissingIdempotencyKey_400(t *testing.T) {
	a := newAPI(t)
	token := a.login(t, "student01")
	start, _ := testutil.Slot18()

	resp := a.do(t, request{
		method:      http.MethodPost,
		path:        "/api/v1/bookings",
		token:       token,
		omitIdemKey: true,
		body: map[string]any{
			"facility_id":      testutil.CourtID().String(),
			"start":            start.Format(time.RFC3339),
			"duration_minutes": 60,
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Equal(t, httpx.CodeBadRequest, resp.errorBody(t).Error)

	var n int
	require.NoError(t, a.pg.Pool.QueryRow(context.Background(), `SELECT count(*) FROM bookings`).Scan(&n))
	require.Equal(t, 0, n, "a rejected request must not reach the database")
}

func TestHTTP_MalformedIdempotencyKey_400(t *testing.T) {
	a := newAPI(t)
	token := a.login(t, "student01")
	start, _ := testutil.Slot18()

	// A constant string is the dangerous case: without validation every submit
	// from this client would collapse onto its first booking, which looks
	// exactly like the system losing bookings.
	// An empty header value is indistinguishable from an absent one at the HTTP
	// layer, so that case belongs to TestHTTP_MissingIdempotencyKey_400.
	for _, key := range []string{"not-a-uuid", "12345", "   ", "0000", uuid.NewString() + "x"} {
		resp := a.do(t, request{
			method:     http.MethodPost,
			path:       "/api/v1/bookings",
			token:      token,
			rawIdemKey: key,
			body: map[string]any{
				"facility_id":      testutil.CourtID().String(),
				"start":            start.Format(time.RFC3339),
				"duration_minutes": 60,
			},
		})
		require.Equalf(t, http.StatusBadRequest, resp.status, "key %q was accepted", key)
		require.Equal(t, httpx.CodeBadRequest, resp.errorBody(t).Error)
	}
}

func TestHTTP_Unauthenticated_401(t *testing.T) {
	a := newAPI(t)
	start, _ := testutil.Slot18()

	body := map[string]any{
		"facility_id":      testutil.CourtID().String(),
		"start":            start.Format(time.RFC3339),
		"duration_minutes": 60,
	}

	t.Run("no token", func(t *testing.T) {
		resp := a.do(t, request{method: http.MethodPost, path: "/api/v1/bookings", body: body})
		require.Equal(t, http.StatusUnauthorized, resp.status)
		require.Equal(t, httpx.CodeUnauthenticated, resp.errorBody(t).Error)
	})

	t.Run("garbage token", func(t *testing.T) {
		resp := a.do(t, request{method: http.MethodPost, path: "/api/v1/bookings",
			token: "not.a.jwt", body: body})
		require.Equal(t, http.StatusUnauthorized, resp.status)
	})

	t.Run("token signed with the wrong key", func(t *testing.T) {
		// HS256 signed with a different secret. Accepting this would make the
		// whole auth layer decorative.
		forged := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
			"eyJzdWIiOiIwMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDEifQ." +
			"c2lnbmF0dXJlLXRoYXQtd2lsbC1ub3QtdmVyaWZ5"
		resp := a.do(t, request{method: http.MethodPost, path: "/api/v1/bookings",
			token: forged, body: body})
		require.Equal(t, http.StatusUnauthorized, resp.status)
	})

	t.Run("reads are protected too", func(t *testing.T) {
		resp := a.do(t, request{method: http.MethodGet, path: "/api/v1/bookings/me"})
		require.Equal(t, http.StatusUnauthorized, resp.status)
	})
}

func TestHTTP_CancelNotOwner_403(t *testing.T) {
	a := newAPI(t)
	start, _ := testutil.Slot18()

	owner := a.login(t, "student01")
	created := a.createBooking(t, owner, testutil.CourtID(), start, 60, uuid.NewString())
	require.Equal(t, http.StatusCreated, created.status)

	var b struct {
		ID uuid.UUID `json:"id"`
	}
	created.decode(t, &b)

	intruder := a.login(t, "student02")
	resp := a.do(t, request{
		method: http.MethodDelete,
		path:   "/api/v1/bookings/" + b.ID.String(),
		token:  intruder,
	})
	require.Equal(t, http.StatusForbidden, resp.status)
	require.Equal(t, httpx.CodeForbidden, resp.errorBody(t).Error)

	// Still confirmed.
	var status string
	require.NoError(t, a.pg.Pool.QueryRow(context.Background(),
		`SELECT status::text FROM bookings WHERE id = $1`, b.ID).Scan(&status))
	require.Equal(t, "CONFIRMED", status)

	// The owner can, and a manager can.
	require.Equal(t, http.StatusOK, a.do(t, request{
		method: http.MethodDelete, path: "/api/v1/bookings/" + b.ID.String(), token: owner,
	}).status)

	// A retried cancel converges: non-negotiable #5 says a retry returns the
	// original result, and a DELETE whose 200 was lost in transit is exactly
	// that. A 409 here would be a scary error for an action that succeeded.
	repeat := a.do(t, request{
		method: http.MethodDelete, path: "/api/v1/bookings/" + b.ID.String(), token: owner,
	})
	require.Equal(t, http.StatusOK, repeat.status,
		"a retried cancel must return the original result: %s", repeat.raw)

	var repeatBody struct {
		ID     uuid.UUID `json:"id"`
		Status string    `json:"status"`
	}
	repeat.decode(t, &repeatBody)
	require.Equal(t, b.ID, repeatBody.ID)
	require.Equal(t, "CANCELLED", repeatBody.Status)

	// Side effects still ran exactly once.
	var events int
	require.NoError(t, a.pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM booking_events WHERE booking_id = $1 AND to_status = 'CANCELLED'`,
		b.ID).Scan(&events))
	require.Equal(t, 1, events)
}

// TestHTTP_CancelRetryIsIdempotent is the lost-response scenario end to end: the
// same DELETE issued repeatedly, as a flaky client would, must be indistinguishable
// from issuing it once.
func TestHTTP_CancelRetryIsIdempotent(t *testing.T) {
	a := newAPI(t)
	start, _ := testutil.Slot18()

	token := a.login(t, "student01")
	created := a.createBooking(t, token, testutil.GymID(), start, 60, uuid.NewString())
	require.Equal(t, http.StatusCreated, created.status)

	var b struct {
		ID uuid.UUID `json:"id"`
	}
	created.decode(t, &b)

	var booked int
	require.NoError(t, a.pg.Pool.QueryRow(context.Background(),
		`SELECT booked FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
		testutil.GymID(), start.UTC()).Scan(&booked))
	require.Equal(t, 1, booked)

	for i := 0; i < 5; i++ {
		resp := a.do(t, request{
			method: http.MethodDelete,
			path:   "/api/v1/bookings/" + b.ID.String(),
			token:  token,
		})
		require.Equalf(t, http.StatusOK, resp.status, "retry %d returned %s", i, resp.raw)
	}

	// The place was returned once, not five times.
	require.NoError(t, a.pg.Pool.QueryRow(context.Background(),
		`SELECT booked FROM slot_capacity WHERE facility_id = $1 AND slot_start = $2`,
		testutil.GymID(), start.UTC()).Scan(&booked))
	require.Equal(t, 0, booked, "the capacity release must fire exactly once across retries")

	var events, outboxRows int
	require.NoError(t, a.pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM booking_events WHERE booking_id = $1 AND to_status = 'CANCELLED'`,
		b.ID).Scan(&events))
	require.Equal(t, 1, events)

	require.NoError(t, a.pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE topic = 'booking.cancelled'`).Scan(&outboxRows))
	require.Equal(t, 1, outboxRows)
}

func TestHTTP_ListMineAndFacilities(t *testing.T) {
	a := newAPI(t)
	token := a.login(t, "student01")
	start, _ := testutil.Slot18()

	require.Equal(t, http.StatusCreated,
		a.createBooking(t, token, testutil.CourtID(), start, 60, uuid.NewString()).status)

	resp := a.do(t, request{method: http.MethodGet, path: "/api/v1/bookings/me", token: token})
	require.Equal(t, http.StatusOK, resp.status)

	var mine struct {
		Upcoming []struct {
			ID       uuid.UUID `json:"id"`
			Facility string    `json:"facility"`
		} `json:"upcoming"`
		Past []struct{} `json:"past"`
	}
	resp.decode(t, &mine)
	require.Len(t, mine.Upcoming, 1)
	require.Equal(t, "Tennis Court 1", mine.Upcoming[0].Facility)
	require.NotNil(t, mine.Past, "empty lists serialise as [], not null")

	facilities := a.do(t, request{method: http.MethodGet, path: "/api/v1/facilities", token: token})
	require.Equal(t, http.StatusOK, facilities.status)

	var cat struct {
		Facilities []struct {
			Name        string `json:"name"`
			IsExclusive bool   `json:"is_exclusive"`
			Capacity    int    `json:"capacity"`
		} `json:"facilities"`
	}
	facilities.decode(t, &cat)
	require.Len(t, cat.Facilities, 7)
}

func TestHTTP_ValidationAndBadRequestsAreDistinct(t *testing.T) {
	a := newAPI(t)
	token := a.login(t, "student01")
	start, _ := testutil.Slot18()

	// Malformed input is a 400: the request could not be understood.
	bad := a.do(t, request{
		method: http.MethodPost, path: "/api/v1/bookings", token: token,
		body: map[string]any{"facility_id": "nope", "start": start.Format(time.RFC3339), "duration_minutes": 60},
	})
	require.Equal(t, http.StatusBadRequest, bad.status)

	// Well-formed input that breaks a rule is a 422: understood, refused.
	offGrid := a.do(t, request{
		method: http.MethodPost, path: "/api/v1/bookings", token: token,
		body: map[string]any{
			"facility_id":      testutil.GymID().String(),
			"start":            start.Add(30 * time.Minute).Format(time.RFC3339),
			"duration_minutes": 60,
		},
	})
	require.Equal(t, http.StatusUnprocessableEntity, offGrid.status)
	require.Equal(t, "SLOT_NOT_ALIGNED", offGrid.errorBody(t).Error,
		"the domain's specific code must survive to the wire")
}

func TestHTTP_ProbesNeedNoAuth(t *testing.T) {
	a := newAPI(t)

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		resp := a.do(t, request{method: http.MethodGet, path: path})
		require.Equalf(t, http.StatusOK, resp.status, "%s should not require auth", path)
	}
}
