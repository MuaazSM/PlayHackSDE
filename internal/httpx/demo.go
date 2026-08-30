package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/demo"
)

// DemoHandlers serve the race console (IMPLEMENTATION.md §13).
//
// Thin, like every other handler here: parse, validate shape, delegate, respond.
// The race itself lives in internal/demo and calls the booking service directly
// in-process — see demo.Run for why nothing about the race goes back out over
// HTTP.
//
// These routes are mounted ONLY in AUTH_MODE=dev; see NewRouter.
type DemoHandlers struct {
	demo *demo.Service
	loc  *time.Location
}

// NewDemoHandlers wires the race console to the HTTP edge.
func NewDemoHandlers(d *demo.Service, loc *time.Location) *DemoHandlers {
	if loc == nil {
		loc = time.UTC
	}
	return &DemoHandlers{demo: d, loc: loc}
}

// demoSlotRequest is the shape both demo endpoints take: a facility and a slot.
type demoSlotRequest struct {
	FacilityID      string `json:"facility_id"`
	Start           string `json:"start"`
	DurationMinutes int    `json:"duration_minutes"`
	N               int    `json:"n"`
}

func (req demoSlotRequest) parse() (uuid.UUID, time.Time, time.Duration, error) {
	facilityID, err := uuid.Parse(req.FacilityID)
	if err != nil {
		return uuid.Nil, time.Time{}, 0, fmt.Errorf("%w: facility_id must be a UUID", ErrBadRequest)
	}
	start, err := time.Parse(time.RFC3339, req.Start)
	if err != nil {
		return uuid.Nil, time.Time{}, 0, fmt.Errorf("%w: start must be an RFC3339 timestamp", ErrBadRequest)
	}

	d := demo.DefaultDuration
	if req.DurationMinutes > 0 {
		d = time.Duration(req.DurationMinutes) * time.Minute
	}
	return facilityID, start, d, nil
}

func decodeDemoRequest(w http.ResponseWriter, r *http.Request) (demoSlotRequest, error) {
	var req demoSlotRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		return req, fmt.Errorf("%w: body must be JSON", ErrBadRequest)
	}
	return req, nil
}

// Race serves POST /api/v1/demo/race.
//
// Fires n concurrent attempts at one slot and returns the outcome split plus the
// database's own count of confirmed bookings for that slot. That count is the
// proof; everything else in the body is telemetry.
func (h *DemoHandlers) Race(w http.ResponseWriter, r *http.Request) {
	req, err := decodeDemoRequest(w, r)
	if err != nil {
		Error(w, r, err)
		return
	}

	facilityID, start, duration, err := req.parse()
	if err != nil {
		Error(w, r, err)
		return
	}

	res, err := h.demo.Run(r.Context(), demo.Request{
		FacilityID: facilityID,
		Start:      start,
		Duration:   duration,
		N:          req.N,
	})
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, res)
}

// Reset serves POST /api/v1/demo/reset.
//
// Cancels the bookings occupying the demo slot so the race can be fired again,
// live, on stage. Returns the same db_count the race returns — after a reset it
// reads zero, which is the console showing the slot go 1 -> 0 -> 1 without ever
// changing which question it asks the database.
func (h *DemoHandlers) Reset(w http.ResponseWriter, r *http.Request) {
	req, err := decodeDemoRequest(w, r)
	if err != nil {
		Error(w, r, err)
		return
	}

	facilityID, start, duration, err := req.parse()
	if err != nil {
		Error(w, r, err)
		return
	}

	res, err := h.demo.Reset(r.Context(), facilityID, start, start.Add(duration))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, res)
}
