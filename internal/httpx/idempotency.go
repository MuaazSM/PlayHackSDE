package httpx

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// HeaderIdempotencyKey is the client-supplied key for a submit intention.
const HeaderIdempotencyKey = "Idempotency-Key"

type idemKeyCtx struct{}

// IdempotencyKeyFrom returns the validated key for this request.
func IdempotencyKeyFrom(ctx context.Context) (string, bool) {
	k, ok := ctx.Value(idemKeyCtx{}).(string)
	return k, ok
}

// RequireIdempotencyKey validates the header on state-CREATING writes.
//
// The key is what makes a retry safe: uq_bookings_user_idem turns a replayed
// submit into the original booking instead of a second one. It must parse as a
// UUID — a client that reuses one constant string across every request would
// otherwise get its second booking silently mapped onto its first, which looks
// exactly like the system losing a booking.
//
// Applied to POST/PUT/PATCH, not DELETE. Cancel is already idempotent by
// construction: its status-guarded UPDATE means a replayed cancel matches zero
// rows and returns 409, so a key there would be required, validated, and then
// ignored — ceremony that suggests a guarantee the key is not providing.
func RequireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			next.ServeHTTP(w, r)
			return
		}

		raw := r.Header.Get(HeaderIdempotencyKey)
		if raw == "" {
			Error(w, r, fmt.Errorf("%w: %s header is required", ErrBadRequest, HeaderIdempotencyKey))
			return
		}
		if _, err := uuid.Parse(raw); err != nil {
			Error(w, r, fmt.Errorf("%w: %s must be a UUID", ErrBadRequest, HeaderIdempotencyKey))
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), idemKeyCtx{}, raw)))
	})
}

// ShedMiddleware bounds concurrent booking writes and rejects the excess
// immediately. Wraps the booking write handler ONLY — reads are never shed.
func (s *Shedder) ShedMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := s.Do(r.Context(), func(ctx context.Context) error {
			next.ServeHTTP(w, r.WithContext(ctx))
			return nil
		})
		if err != nil {
			// Error sets a jittered Retry-After for every 429.
			Error(w, r, err)
		}
	})
}
