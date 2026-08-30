package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/iitg-playhack/sportsbook/internal/booking"
)

// Machine-readable error codes. The client switches on these and never parses
// the human-readable message.
const (
	CodeSlotTaken       = "SLOT_TAKEN"
	CodeCapacityFull    = "CAPACITY_FULL"
	CodeNotCancellable  = "NOT_CANCELLABLE"
	CodeValidation      = "VALIDATION_FAILED"
	CodePolicyLimit     = "POLICY_LIMIT"
	CodeNotFound        = "NOT_FOUND"
	CodeForbidden       = "FORBIDDEN"
	CodeUnauthenticated = "UNAUTHENTICATED"
	CodeShed            = "SERVICE_BUSY"
	CodeRateLimited     = "RATE_LIMITED"
	CodeTimeout         = "TIMEOUT"
	CodeBadRequest      = "BAD_REQUEST"
	CodeInternal        = "INTERNAL"
)

// Alternative is another slot the user could take instead. Populated in Phase 7;
// the field exists now so the envelope shape does not change under clients.
type Alternative struct {
	FacilityID string `json:"facility_id"`
	Name       string `json:"name"`
	Start      string `json:"start"`
}

// ErrorBody is the ONE error envelope, per IMPLEMENTATION.md §10.3.
//
// Every non-2xx response in this service has this shape. No handler writes its
// own error JSON — a client that must special-case the shape of a failure by
// endpoint cannot handle failures generically, which is exactly when it matters.
type ErrorBody struct {
	Error             string        `json:"error"`
	Message           string        `json:"message"`
	Alternatives      []Alternative `json:"alternatives,omitempty"`
	WaitlistAvailable *bool         `json:"waitlist_available,omitempty"`
	RequestID         string        `json:"request_id"`
}

// JSON writes a success body.
func JSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// Error maps a domain error onto a status code and the single envelope.
//
// This is the only place that decision is made. Handlers return errors; they do
// not choose status codes, and they never format a body.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := classify(err)

	if status >= 500 {
		// A 5xx is a defect, not an outcome. Log the cause; do not leak it.
		slog.ErrorContext(r.Context(), "request failed",
			"err", err,
			"path", r.URL.Path,
			"method", r.Method,
			"request_id", middleware.GetReqID(r.Context()))
	}

	if status == http.StatusTooManyRequests {
		// Jittered, so shed clients do not all return at the same instant —
		// that would just be the herd arriving again on a timer.
		w.Header().Set("Retry-After", strconv.Itoa(int(RetryAfter().Round(time.Second).Seconds())))
	}

	JSON(w, status, ErrorBody{
		Error:     code,
		Message:   message,
		RequestID: middleware.GetReqID(r.Context()),
	})
}

func classify(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, booking.ErrSlotTaken):
		// Phase 7 fills in alternatives; the code and status are already final.
		return http.StatusConflict, CodeSlotTaken, "That slot was taken moments ago."

	case errors.Is(err, booking.ErrCapacityFull):
		return http.StatusConflict, CodeCapacityFull, "That session is full."

	case errors.Is(err, booking.ErrNotCancellable):
		return http.StatusConflict, CodeNotCancellable, "That booking is no longer active."

	case errors.Is(err, booking.ErrPolicyExceeded):
		return http.StatusUnprocessableEntity, CodePolicyLimit, err.Error()

	case errors.Is(err, booking.ErrValidation):
		c := booking.Code(err)
		if c == "" {
			c = CodeValidation
		}
		return http.StatusUnprocessableEntity, c, err.Error()

	case errors.Is(err, booking.ErrNotFound):
		return http.StatusNotFound, CodeNotFound, "Not found."

	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized, CodeUnauthenticated, "Sign in to continue."

	case errors.Is(err, booking.ErrForbidden):
		return http.StatusForbidden, CodeForbidden, "That booking belongs to someone else."

	case errors.Is(err, booking.ErrShed):
		return http.StatusTooManyRequests, CodeShed, "Too many bookings at once — try again in a moment."

	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests, CodeRateLimited, "Too many requests — slow down."

	case errors.Is(err, ErrBadRequest):
		return http.StatusBadRequest, CodeBadRequest, err.Error()

	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, CodeTimeout, "The request took too long."

	default:
		// Unmapped means unknown, and unknown is a 500. Guessing a friendlier
		// status here would report a defect as an outcome.
		return http.StatusInternalServerError, CodeInternal, "Something went wrong."
	}
}

// Edge errors, raised by middleware rather than the domain.
var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrRateLimited     = errors.New("rate limited")
	ErrBadRequest      = errors.New("bad request")
)
