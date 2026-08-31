package httpx

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/iitg-playhack/sportsbook/internal/observability"
)

// bookingWriteRoute is the chi route pattern of the one write that has a latency
// budget attached to it. Matched on the PATTERN, not the raw path, so this stays
// one series rather than one per facility.
const bookingWriteRoute = "/api/v1/bookings"

// Metrics records booking_write_duration_seconds{outcome} for the write path.
//
// Measured HERE, at the HTTP edge, rather than around the service call: the
// targets in CLAUDE.md are p99s a student experiences, and admission control is
// what makes losing fast. A timer that started after the shedder would measure
// only the requests that were never shed and report the fast path as the whole
// population.
//
// Placed early in the chain so shed and rate-limited responses are counted too.
// A middleware that only sees successful requests measures the wrong population
// — under load, most requests are not successful, and that is the point.
//
// Only the booking write is observed. Everything else the API serves either has
// its own metric (availability_query_duration_seconds) or has no budget anybody
// is watching during the surge, and §14 is a closed list of eight.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		started := time.Now()

		next.ServeHTTP(ww, r)

		if r.Method != http.MethodPost {
			return
		}
		if chi.RouteContext(r.Context()).RoutePattern() != bookingWriteRoute {
			return
		}

		observability.ObserveWrite(writeOutcome(ww.Status()), time.Since(started))
	})
}

// writeOutcome maps a status to one of the three outcomes §14 splits on.
//
// 200 counts as confirmed: an idempotent replay returns a booking the caller
// owns, and calling that anything else would understate how many students walked
// away with a court.
//
// Everything that is not 200/201/409/429 — validation, auth, 500 — returns "",
// which ObserveWrite drops. A histogram of the latency targets should not be
// diluted by a 422 that never touched the database.
func writeOutcome(status int) string {
	switch status {
	case http.StatusCreated, http.StatusOK:
		return observability.OutcomeConfirmed
	case http.StatusConflict:
		return observability.OutcomeConflict
	case http.StatusTooManyRequests:
		return observability.OutcomeShed
	default:
		return ""
	}
}
