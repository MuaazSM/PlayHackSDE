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
	"github.com/iitg-playhack/sportsbook/internal/checkin"
	"github.com/iitg-playhack/sportsbook/internal/demo"
	"github.com/iitg-playhack/sportsbook/internal/waitlist"
)

// Machine-readable error codes. The client switches on these and never parses
// the human-readable message.
const (
	CodeSlotTaken       = "SLOT_TAKEN"
	CodeCapacityFull    = "CAPACITY_FULL"
	CodeNotCancellable  = "NOT_CANCELLABLE"
	CodeOfferExpired    = "OFFER_EXPIRED"
	CodeAlreadyWaiting  = "ALREADY_WAITING"
	CodeNotWaiting      = "NOT_WAITING"
	CodeCheckinToken    = "CHECKIN_TOKEN_INVALID"
	CodeCheckinWindow   = "CHECKIN_WINDOW"
	CodeNotCheckable    = "NOT_CHECKABLE"
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

// Alternative is another slot the user could take instead.
//
// Three fields, exactly as §10.3 prints them, and start is a local HH:MM because
// that is what the contract shows and what a student reads. It is not lossy in
// practice: every alternative is on the same local day as the request they just
// lost, which the client already knows.
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

	body := ErrorBody{
		Error:     code,
		Message:   message,
		RequestID: middleware.GetReqID(r.Context()),
	}

	// A conflict knows where else the user could go. Everything else omits both
	// fields rather than sending an empty list — §10.3: they are absent where
	// meaningless, so a client can test presence instead of emptiness.
	var conflict *booking.Conflict
	if errors.As(err, &conflict) {
		body.Message = conflictMessage(conflict, code, message)
		body.Alternatives = renderAlternatives(conflict.Alternatives, DisplayLocation(r.Context()))
		body.WaitlistAvailable = &conflict.WaitlistAvailable
	}

	JSON(w, status, body)
}

// conflictMessage names the reason, per §10.3. The facility is what the student
// was looking at, so saying it back is the difference between "something went
// wrong" and "that court is gone".
func conflictMessage(c *booking.Conflict, code, fallback string) string {
	if c.FacilityName == "" {
		return fallback
	}
	if code == CodeCapacityFull {
		return c.FacilityName + " is full for that time."
	}
	return c.FacilityName + " was booked moments ago."
}

// renderAlternatives converts domain suggestions to the wire shape, localising
// the start at the edge — the only place localisation happens (CLAUDE.md).
func renderAlternatives(alts []booking.Alternative, loc *time.Location) []Alternative {
	if len(alts) == 0 {
		return nil
	}
	out := make([]Alternative, 0, len(alts))
	for _, a := range alts {
		out = append(out, Alternative{
			FacilityID: a.FacilityID.String(),
			Name:       a.Name,
			Start:      a.Start.In(loc).Format("15:04"),
		})
	}
	return out
}

func classify(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, booking.ErrSlotTaken):
		// The fallback prose. A *booking.Conflict replaces it with one that names
		// the facility; this is what a conflict raised without one still says.
		return http.StatusConflict, CodeSlotTaken, "That slot was taken moments ago."

	case errors.Is(err, booking.ErrCapacityFull):
		return http.StatusConflict, CodeCapacityFull, "That session is full."

	case errors.Is(err, booking.ErrNotCancellable):
		return http.StatusConflict, CodeNotCancellable, "That booking is no longer active."

	case errors.Is(err, booking.ErrOfferExpired):
		return http.StatusConflict, CodeOfferExpired,
			"That offer has expired — the slot went to the next student in the queue."

	// Check-in (§7). The token failure is a 403 rather than a 422: the request
	// is well formed, the caller simply cannot prove they are at the venue.
	case errors.Is(err, checkin.ErrInvalidToken):
		return http.StatusForbidden, CodeCheckinToken,
			"That code is not valid here — scan the one on the venue display."

	case errors.Is(err, checkin.ErrOutsideWindow):
		return http.StatusConflict, CodeCheckinWindow,
			"Check-in opens ten minutes before your slot and closes shortly after it starts."

	case errors.Is(err, checkin.ErrNotCheckable):
		return http.StatusConflict, CodeNotCheckable, "That booking can no longer be checked into."

	case errors.Is(err, waitlist.ErrAlreadyWaiting):
		return http.StatusConflict, CodeAlreadyWaiting, "You are already in the queue for that slot."

	case errors.Is(err, waitlist.ErrNotWaiting):
		return http.StatusConflict, CodeNotWaiting, "That queue entry is no longer waiting."

	case errors.Is(err, waitlist.ErrValidation):
		return http.StatusUnprocessableEntity, CodeValidation, err.Error()

	case errors.Is(err, waitlist.ErrNotFound):
		return http.StatusNotFound, CodeNotFound, "Not found."

	case errors.Is(err, waitlist.ErrForbidden):
		return http.StatusForbidden, CodeForbidden, "That queue entry belongs to someone else."

	case errors.Is(err, booking.ErrPolicyExceeded):
		return http.StatusUnprocessableEntity, CodePolicyLimit, err.Error()

	// The race console's own request errors — a malformed n, an unknown
	// facility, or a database nobody has seeded yet. Same class as a validation
	// failure: the caller asked for something that cannot be run, and the
	// message says what to change.
	case errors.Is(err, demo.ErrInvalid), errors.Is(err, demo.ErrNoBookers):
		return http.StatusUnprocessableEntity, CodeValidation, err.Error()

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

	// A role the caller does not hold. Distinct from the sentence above because
	// the two are different facts: one is "not yours", the other is "not your
	// job", and telling a student their manager console is somebody else's
	// booking would be nonsense.
	case errors.Is(err, ErrForbiddenRole):
		return http.StatusForbidden, CodeForbidden, "You do not have permission to do that."

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

	// ErrForbiddenRole means the caller is authenticated but lacks the role a
	// route requires. Raised by RequireRole.
	ErrForbiddenRole = errors.New("role not permitted")
)
