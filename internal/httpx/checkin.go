package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/checkin"
)

// RoleManager may operate the venue display. Spelled once, here, matching the
// user_role enum from migration 0002.
const RoleManager = "MANAGER"

// RequireRole rejects a caller whose token does not carry the given role.
//
// Placed AFTER Auth in the chain, which is the only order that makes sense: the
// role comes from the verified token, and a middleware that ran first would have
// nothing to read. Returns 403 rather than 404 — the route exists, this caller
// may not use it.
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				Error(w, r, ErrUnauthenticated)
				return
			}
			if p.Role != role {
				Error(w, r, fmt.Errorf("%w: %s only", ErrForbiddenRole, role))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CheckinHandlers are the two endpoints of IMPLEMENTATION.md §7. Thin, like
// every other handler here: parse, delegate, map, respond.
type CheckinHandlers struct {
	svc *checkin.Service
}

// NewCheckinHandlers wires the HTTP edge to the check-in domain.
func NewCheckinHandlers(svc *checkin.Service) *CheckinHandlers {
	return &CheckinHandlers{svc: svc}
}

// checkinTokenResponse is what the venue display renders as a QR code.
//
// refresh_in_seconds is how long this token stays CURRENT; valid_for_seconds is
// how long it will still be ACCEPTED, which is one window longer because
// verification takes the previous window too. The display re-renders on the
// first number; the second is what makes a slow scan work anyway.
type checkinTokenResponse struct {
	FacilityID       uuid.UUID `json:"facility_id"`
	Token            string    `json:"token"`
	RefreshInSeconds int       `json:"refresh_in_seconds"`
	ValidForSeconds  int       `json:"valid_for_seconds"`
	IssuedAt         time.Time `json:"issued_at"`
}

// FacilityToken serves GET /api/v1/facilities/:id/checkin-token.
//
// MANAGER only. The token is a bearer proof of physical presence at the venue,
// so handing it to a student over the API would defeat the entire point — they
// could check in from the hostel. The display polls this; nobody else may.
func (h *CheckinHandlers) FacilityToken(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, fmt.Errorf("%w: facility id must be a UUID", ErrBadRequest))
		return
	}

	minter := h.svc.Minter()
	token := minter.Mint(id)
	if token == "" {
		// No secret configured. Fail closed and say so, rather than serving a
		// token signed with an empty key that anybody could recompute.
		Error(w, r, fmt.Errorf("%w: CHECKIN_HMAC_SECRET is not configured", ErrBadRequest))
		return
	}

	refresh := minter.ExpiresIn()
	JSON(w, http.StatusOK, checkinTokenResponse{
		FacilityID:       id,
		Token:            token,
		RefreshInSeconds: int(refresh.Round(time.Second).Seconds()),
		ValidForSeconds:  int((refresh + checkin.Window).Round(time.Second).Seconds()),
		IssuedAt:         time.Now().UTC(),
	})
}

type checkInRequest struct {
	Token string `json:"token"`
}

// checkInResponse restates the booking that was attended, so the student's
// screen can show what they just checked into rather than a bare 200.
type checkInResponse struct {
	BookingID   uuid.UUID `json:"booking_id"`
	Reference   string    `json:"reference"`
	FacilityID  uuid.UUID `json:"facility_id"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	CheckedInAt time.Time `json:"checked_in_at"`
	Method      string    `json:"method"`
}

// CheckIn serves POST /api/v1/bookings/:id/check-in.
//
//	200 on a recorded attendance, and on a repeated scan
//	403 wrong venue's token, or somebody else's booking
//	409 outside the check-in window, or a booking that cannot be attended
//	404 no such booking
//
// No Idempotency-Key middleware, deliberately. The primary key on
// check_ins(booking_id) is a STRONGER guarantee than a client-supplied key: it
// makes a second check-in impossible regardless of what any client sends, and it
// holds across devices, so a student scanning on a second phone converges rather
// than double-recording. Non-negotiable #5 is satisfied by the schema here, not
// by a header — the same way DELETE /bookings/:id converges on its status guard.
func (h *CheckinHandlers) CheckIn(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		Error(w, r, ErrUnauthenticated)
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, fmt.Errorf("%w: booking id must be a UUID", ErrBadRequest))
		return
	}

	var req checkInRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		Error(w, r, fmt.Errorf("%w: body must be JSON", ErrBadRequest))
		return
	}

	att, err := h.svc.Redeem(r.Context(), id, p.UserID, req.Token)
	if err != nil {
		Error(w, r, err)
		return
	}

	JSON(w, http.StatusOK, checkInResponse{
		BookingID:   att.BookingID,
		Reference:   att.Reference,
		FacilityID:  att.FacilityID,
		Start:       att.Start,
		End:         att.End,
		CheckedInAt: att.At,
		Method:      att.Method,
	})
}
