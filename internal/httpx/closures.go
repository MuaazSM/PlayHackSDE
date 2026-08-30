package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
)

// Manager closures, IMPLEMENTATION.md §10.4. Three thin handlers over
// booking.Service: a closure is a booking row, so it is the booking domain that
// owns it and nothing here decides what gets blocked.
//
// All three are MANAGER-only, enforced by RequireRole at the route (router.go)
// rather than in these functions — the same place every other role check lives.

type createClosureRequest struct {
	FacilityID      string `json:"facility_id"`
	Start           string `json:"start"`
	End             string `json:"end"`
	DurationMinutes int    `json:"duration_minutes"`
	Reason          string `json:"reason"`
}

// affectedBookingResponse is one student a human has to deal with.
type affectedBookingResponse struct {
	BookingID string    `json:"booking_id"`
	UserID    string    `json:"user_id"`
	RollNo    string    `json:"roll_no"`
	Name      string    `json:"name"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Status    string    `json:"status"`
}

// closureResponse is the wire shape of a blocked window.
//
// affected_bookings is always present, even empty. Unlike the error envelope's
// optional members, this one is the answer to a question the manager asked by
// creating the closure — "who does this affect?" — and an absent key would read
// as "unknown" rather than as "nobody".
type closureResponse struct {
	ID         uuid.UUID                 `json:"id"`
	FacilityID uuid.UUID                 `json:"facility_id"`
	Facility   string                    `json:"facility,omitempty"`
	Start      time.Time                 `json:"start"`
	End        time.Time                 `json:"end"`
	Status     string                    `json:"status"`
	Reason     string                    `json:"reason,omitempty"`
	CreatedAt  time.Time                 `json:"created_at"`
	Slots      int                       `json:"slots_closed,omitempty"`
	Affected   []affectedBookingResponse `json:"affected_bookings"`
}

func toClosureResponse(c *booking.Closure) closureResponse {
	out := closureResponse{
		ID:         c.ID,
		FacilityID: c.FacilityID,
		Facility:   c.FacilityName,
		Start:      c.Start,
		End:        c.End,
		Status:     c.Status,
		Reason:     c.Reason,
		CreatedAt:  c.CreatedAt,
		Slots:      c.Slots,
		Affected:   make([]affectedBookingResponse, 0, len(c.Affected)),
	}
	for _, a := range c.Affected {
		out.Affected = append(out.Affected, affectedBookingResponse{
			BookingID: a.ID.String(),
			UserID:    a.UserID.String(),
			RollNo:    a.RollNo,
			Name:      a.Name,
			Start:     a.Start,
			End:       a.End,
			Status:    a.Status,
		})
	}
	return out
}

// CreateClosure serves POST /api/v1/closures.
//
//	201 on a new closure, carrying the bookings staff must resolve
//	200 on a replayed request, carrying the original closure
//	409 bookings already occupy the window (exclusive facilities), listing them
//	404 no such facility
//	422 validation
//
// The window may be given as start + end, or as start + duration_minutes. Both
// spellings exist because the manager console posts a range and the availability
// grid posts a slot, and making one of them convert would put date arithmetic in
// a screen.
func (h *Handlers) CreateClosure(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		Error(w, r, ErrUnauthenticated)
		return
	}

	var req createClosureRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		Error(w, r, fmt.Errorf("%w: body must be JSON", ErrBadRequest))
		return
	}

	facilityID, err := uuid.Parse(req.FacilityID)
	if err != nil {
		Error(w, r, fmt.Errorf("%w: facility_id must be a UUID", ErrBadRequest))
		return
	}

	start, err := time.Parse(time.RFC3339, req.Start)
	if err != nil {
		Error(w, r, fmt.Errorf("%w: start must be an RFC3339 timestamp", ErrBadRequest))
		return
	}

	var end time.Time
	switch {
	case req.End != "":
		end, err = time.Parse(time.RFC3339, req.End)
		if err != nil {
			Error(w, r, fmt.Errorf("%w: end must be an RFC3339 timestamp", ErrBadRequest))
			return
		}
	case req.DurationMinutes > 0:
		end = start.Add(time.Duration(req.DurationMinutes) * time.Minute)
	default:
		Error(w, r, fmt.Errorf("%w: end or duration_minutes is required", ErrBadRequest))
		return
	}

	c, err := h.bookings.CreateClosure(r.Context(), booking.ClosureRequest{
		FacilityID: facilityID,
		ActorID:    p.UserID,
		Start:      start,
		End:        end,
		Reason:     req.Reason,
	})
	if err != nil {
		Error(w, r, err)
		return
	}

	// A replay is a success the caller already has, exactly as on the booking
	// write path: 200 with the original body, so the console can tell "closed
	// now" from "was already closed".
	status := http.StatusCreated
	if c.Replayed {
		status = http.StatusOK
	}
	JSON(w, status, toClosureResponse(c))
}

// ReopenClosure serves DELETE /api/v1/closures/:id.
//
// The window becomes bookable again the instant the transaction commits, and for
// a shared facility the capacity counters the closure zeroed are restored.
//
// No Idempotency-Key, for the same reason DELETE /bookings/:id has none: the
// status-guarded UPDATE already makes a replay converge on the original result,
// which is a stronger guarantee than a client-supplied header.
func (h *Handlers) ReopenClosure(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		Error(w, r, ErrUnauthenticated)
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, fmt.Errorf("%w: closure id must be a UUID", ErrBadRequest))
		return
	}

	c, err := h.bookings.Reopen(r.Context(), id, p.UserID, r.URL.Query().Get("reason"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, toClosureResponse(c))
}

// ListClosures serves GET /api/v1/closures?facility_id=&date= — the manager
// console's board of what is currently closed.
func (h *Handlers) ListClosures(w http.ResponseWriter, r *http.Request) {
	var filter booking.ClosureFilter

	if raw := r.URL.Query().Get("facility_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			Error(w, r, fmt.Errorf("%w: facility_id must be a UUID", ErrBadRequest))
			return
		}
		filter.FacilityID = id
	}

	// Unlike the availability endpoints, an absent date means EVERY day rather
	// than today: a manager opening the console needs to see the closure they
	// scheduled for next Tuesday, not an empty screen.
	if raw := r.URL.Query().Get("date"); raw != "" {
		if _, err := time.ParseInLocation("2006-01-02", raw, h.loc); err != nil {
			Error(w, r, fmt.Errorf("%w: date must be YYYY-MM-DD", ErrBadRequest))
			return
		}
		filter.Date = raw
	}

	list, err := h.bookings.ListClosures(r.Context(), filter)
	if err != nil {
		Error(w, r, err)
		return
	}

	out := make([]closureResponse, 0, len(list))
	for i := range list {
		out = append(out, toClosureResponse(&list[i]))
	}
	JSON(w, http.StatusOK, map[string]any{"closures": out})
}
