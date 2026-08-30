package httpx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics for the write path. Labelled by route pattern rather than raw path, so
// /api/v1/bookings/{id} is one series instead of one per booking.
var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "playhack_http_requests_total",
		Help: "HTTP requests by route and status.",
	}, []string{"method", "route", "status"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "playhack_http_request_duration_seconds",
		Help: "HTTP request latency by route and status.",
		// Buckets chosen around the targets that matter: rejections under
		// 150ms, confirmations under 250ms. Default buckets put both in the
		// same bin and would hide the split entirely.
		Buckets: []float64{.005, .01, .025, .05, .1, .15, .25, .5, 1, 2.5},
	}, []string{"method", "route", "status"})
)

// Metrics records every request. Placed early in the chain so shed and
// rate-limited responses are counted too — those are the ones worth watching
// under load, and a middleware that only sees successful requests measures the
// wrong population.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		started := time.Now()

		next.ServeHTTP(ww, r)

		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		status := strconv.Itoa(ww.Status())

		requestsTotal.WithLabelValues(r.Method, route, status).Inc()
		requestDuration.WithLabelValues(r.Method, route, status).Observe(time.Since(started).Seconds())
	})
}
