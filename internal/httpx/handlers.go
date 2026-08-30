package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/facility"
)

// Handlers are deliberately thin: parse, validate shape, delegate, map, respond.
// Business logic lives in internal/booking; nothing here decides who wins a slot.
type Handlers struct {
	bookings     *booking.Service
	facilities   *facility.Repo
	availability *facility.Availability
	loc          *time.Location
}

// NewHandlers wires the HTTP edge to the domain.
func NewHandlers(bookings *booking.Service, facilities *facility.Repo, availability *facility.Availability, loc *time.Location) *Handlers {
	if loc == nil {
		loc = time.UTC
	}
	return &Handlers{bookings: bookings, facilities: facilities, availability: availability, loc: loc}
}

// parseDate reads ?date=YYYY-MM-DD, defaulting to today in the campus timezone.
//
// "Today" is resolved in IST, not UTC. At 05:00 IST the UTC date is still
// yesterday, so a UTC default would show a student the wrong day's grid every
// morning — and the bug would look like missing data, not like a timezone
// mistake.
func (h *Handlers) parseDate(r *http.Request) (string, error) {
	raw := r.URL.Query().Get("date")
	if raw == "" || raw == "today" {
		return time.Now().In(h.loc).Format("2006-01-02"), nil
	}
	if _, err := time.ParseInLocation("2006-01-02", raw, h.loc); err != nil {
		return "", fmt.Errorf("%w: date must be YYYY-MM-DD", ErrBadRequest)
	}
	return raw, nil
}

// FacilityAvailability serves GET /api/v1/facilities/:id/availability?date=
func (h *Handlers) FacilityAvailability(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, fmt.Errorf("%w: facility id must be a UUID", ErrBadRequest))
		return
	}

	date, err := h.parseDate(r)
	if err != nil {
		Error(w, r, err)
		return
	}

	f, err := h.facilities.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, facility.ErrNotFound) {
			Error(w, r, fmt.Errorf("%w: facility %s", booking.ErrNotFound, id))
			return
		}
		Error(w, r, err)
		return
	}

	day, err := h.availability.ForFacility(r.Context(), f, date)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, day)
}

// CampusAvailability serves GET /api/v1/availability?date=
//
// One request for the whole discovery screen (FR-02, G-1).
func (h *Handlers) CampusAvailability(w http.ResponseWriter, r *http.Request) {
	date, err := h.parseDate(r)
	if err != nil {
		Error(w, r, err)
		return
	}

	grid, err := h.availability.Campus(r.Context(), date)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, grid)
}

// bookingResponse is the wire shape of a booking.
type bookingResponse struct {
	ID         uuid.UUID `json:"id"`
	Reference  string    `json:"reference"`
	FacilityID uuid.UUID `json:"facility_id"`
	Facility   string    `json:"facility,omitempty"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Status     string    `json:"status"`
}

func toBookingResponse(b *booking.Booking) bookingResponse {
	return bookingResponse{
		ID:         b.ID,
		Reference:  b.Reference,
		FacilityID: b.FacilityID,
		Facility:   b.FacilityName,
		Start:      b.Start,
		End:        b.End,
		Status:     b.Status,
	}
}

type createBookingRequest struct {
	FacilityID      string `json:"facility_id"`
	Start           string `json:"start"`
	DurationMinutes int    `json:"duration_minutes"`
}

// CreateBooking is the write path. POST /api/v1/bookings.
//
//	201 on a new booking
//	200 on an idempotent replay, carrying the ORIGINAL booking
//	409 slot taken / capacity full
//	422 validation
//	429 shed (Retry-After set by the envelope)
func (h *Handlers) CreateBooking(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		Error(w, r, ErrUnauthenticated)
		return
	}

	idemKey, ok := IdempotencyKeyFrom(r.Context())
	if !ok {
		// The middleware guarantees this; belt and braces so a routing mistake
		// cannot silently disable idempotency on the one endpoint that needs it.
		Error(w, r, fmt.Errorf("%w: %s header is required", ErrBadRequest, HeaderIdempotencyKey))
		return
	}

	var req createBookingRequest
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
	if req.DurationMinutes <= 0 {
		Error(w, r, fmt.Errorf("%w: duration_minutes must be positive", ErrBadRequest))
		return
	}

	b, err := h.bookings.Create(r.Context(), booking.CreateRequest{
		FacilityID: facilityID,
		UserID:     p.UserID,
		Start:      start,
		Duration:   time.Duration(req.DurationMinutes) * time.Minute,
		IdemKey:    idemKey,
	})
	if err != nil {
		Error(w, r, err)
		return
	}

	// A replay is a success the client already has. 200 with the original body
	// lets it tell "you booked this now" from "you had already booked this".
	status := http.StatusCreated
	if b.Replayed {
		status = http.StatusOK
	}
	JSON(w, status, toBookingResponse(b))
}

// ListMyBookings serves GET /api/v1/bookings/me.
func (h *Handlers) ListMyBookings(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		Error(w, r, ErrUnauthenticated)
		return
	}

	list, err := h.bookings.ListMine(r.Context(), p.UserID)
	if err != nil {
		Error(w, r, err)
		return
	}

	out := struct {
		Upcoming []bookingResponse `json:"upcoming"`
		Past     []bookingResponse `json:"past"`
	}{
		Upcoming: make([]bookingResponse, 0, len(list.Upcoming)),
		Past:     make([]bookingResponse, 0, len(list.Past)),
	}
	for i := range list.Upcoming {
		out.Upcoming = append(out.Upcoming, toBookingResponse(&list.Upcoming[i]))
	}
	for i := range list.Past {
		out.Past = append(out.Past, toBookingResponse(&list.Past[i]))
	}

	JSON(w, http.StatusOK, out)
}

// CancelBooking serves DELETE /api/v1/bookings/:id.
func (h *Handlers) CancelBooking(w http.ResponseWriter, r *http.Request) {
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

	b, err := h.bookings.Cancel(r.Context(), id, p.UserID, r.URL.Query().Get("reason"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, toBookingResponse(b))
}

type facilityResponse struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	Sport           string    `json:"sport"`
	IsExclusive     bool      `json:"is_exclusive"`
	Capacity        int       `json:"capacity"`
	OpensAt         string    `json:"opens_at"`
	ClosesAt        string    `json:"closes_at"`
	SlotMinutes     int       `json:"slot_minutes"`
	MinDurationMins int       `json:"min_duration_minutes"`
	MaxDurationMins int       `json:"max_duration_minutes"`
}

// ListFacilities serves GET /api/v1/facilities.
func (h *Handlers) ListFacilities(w http.ResponseWriter, r *http.Request) {
	fs, err := h.facilities.List(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}

	out := make([]facilityResponse, 0, len(fs))
	for _, f := range fs {
		out = append(out, facilityResponse{
			ID:              f.ID,
			Name:            f.Name,
			Sport:           f.Sport,
			IsExclusive:     f.IsExclusive,
			Capacity:        f.Capacity,
			OpensAt:         hhmm(f.OpensAt),
			ClosesAt:        hhmm(f.ClosesAt),
			SlotMinutes:     int(f.Granularity.Minutes()),
			MinDurationMins: int(f.MinDuration.Minutes()),
			MaxDurationMins: int(f.MaxDuration.Minutes()),
		})
	}

	JSON(w, http.StatusOK, map[string]any{"facilities": out})
}

func hhmm(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}
