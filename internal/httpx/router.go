package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/checkin"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/demo"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/live"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/waitlist"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

// RouterDeps is everything the HTTP layer needs. Passed in rather than
// constructed here, so tests wire the same router against test doubles.
type RouterDeps struct {
	Config       *config.Config
	DB           *store.DB
	Redis        *redis.Client
	Bookings     *booking.Service
	Facilities   *facility.Repo
	Availability *facility.Availability

	// Waitlist is optional. Left nil, the router builds one over the same
	// database rather than serving a 404 for the queue endpoints — the routes
	// exist as long as the API does, and a caller that has already wired the
	// promotion hook passes its own so both halves share one service.
	Waitlist *waitlist.Service

	// Checkin backs the venue QR and the no-show release (§7). Optional, and
	// left nil the router builds one over the same database — but WITHOUT the
	// promotion hook, because the hook belongs to whoever also runs the sweeper.
	// A binary that wants a no-show to promote off the queue passes its own,
	// wired to the SAME waitlist.Service the cancel path uses.
	Checkin *checkin.Service

	// Demo backs the race console (§13). Optional: left nil the router builds
	// one over the SAME booking service the rest of the API uses, which is the
	// point — a race console wired to its own service would be demonstrating
	// that service rather than this one.
	Demo *demo.Service

	// Live backs GET /api/v1/stream (§9). Optional, but a hub is only useful if
	// somebody is running it: left nil the router builds one over d.Redis that
	// NOBODY HAS STARTED, so the route serves heartbeats and no events. That is
	// the honest degraded mode rather than a 404 — a client that connects and
	// polls is correct, just not live — and it is warned about at boot. Binaries
	// pass their own and call Run on it.
	Live *live.Hub

	// StreamHeartbeat overrides the SSE comment interval. Zero uses
	// StreamHeartbeat. Tests set it short; nothing in production should.
	StreamHeartbeat time.Duration

	Logger *slog.Logger
}

// NewRouter builds the API.
//
// MIDDLEWARE ORDER IS LOAD-BEARING (§10.1):
//
//	RequestID -> Recover -> Metrics -> CORS -> RateLimit(IP) -> Auth
//	  -> RateLimit(user) -> Idempotency -> Shed -> Timeout -> handler
//
// Rate limiting is split around Auth on purpose. Limiting only AFTER auth would
// spend a JWT verification on every request of an unauthenticated flood — the
// flood gets to choose how much CPU it costs us. Limiting only BEFORE auth would
// bucket the whole campus behind one NAT address together, so one abusive client
// could exhaust the budget for everyone sharing it. So: a coarse IP bucket first
// to make floods cheap to reject, then a per-user bucket once we know who this
// is. Two buckets, two Redis keys.
//
// Shed wraps the booking write ONLY. Reads are never shed: availability is cheap
// and cacheable, and serving it during a burst is what keeps the screen honest
// about who won.
func NewRouter(d RouterDeps) http.Handler {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}

	auth := NewAuthenticator(d.Config, d.DB.Primary)
	limiter := NewRateLimiter(d.Redis, time.Minute, log)
	shedder := NewShedder(d.Config.WriteQueueDepth, d.Config.WriteTimeout)
	loc, err := time.LoadLocation(d.Config.TZDisplay)
	if err != nil {
		loc = time.UTC
	}
	wl := d.Waitlist
	if wl == nil {
		wl = waitlist.NewService(d.DB, d.Facilities, d.Config.PromotionTTL, log)
	}
	h := NewHandlers(d.Bookings, d.Facilities, d.Availability, wl, loc)

	ci := d.Checkin
	if ci == nil {
		ci = checkin.NewService(d.DB, d.Facilities,
			checkin.NewMinter(d.Config.CheckinHMACSecret), d.Config.GracePeriod, log)
	}
	if !ci.Minter().Enabled() {
		// Fail closed and say so at boot. An empty secret means every check-in
		// is refused, which is the right behaviour and the wrong surprise to
		// discover at the venue door.
		log.Warn("CHECKIN_HMAC_SECRET is not set; check-in will refuse every token",
			"route", "POST /api/v1/bookings/{id}/check-in")
	}
	checkinHandlers := NewCheckinHandlers(ci)

	dm := d.Demo
	if dm == nil {
		dm = demo.NewService(d.DB, d.Bookings, d.Facilities)
	}
	demoHandlers := NewDemoHandlers(dm, loc)

	hub := d.Live
	if hub == nil {
		log.Warn("no live hub supplied; SSE clients will receive heartbeats only",
			"route", "GET /api/v1/stream")
		hub = live.NewHub(d.Redis, log)
	}
	sse := NewSSE(hub, loc).WithHeartbeat(d.StreamHeartbeat)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(Metrics)
	r.Use(cors)
	// Outermost, so every response — including one written by Recoverer's
	// fallback or by a probe — renders times in the same zone.
	r.Use(withDisplayLocation(loc))

	// Probes and metrics sit outside auth AND outside rate limiting: a liveness
	// probe that can be rate limited will eventually get the container killed
	// during exactly the burst it exists to survive.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", readyz(d.DB, d.Redis))
	r.Handle("/metrics", promhttp.Handler())

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(limiter.ByIP(d.Config.RateLimitIPPerMin))

		// Registered ONLY in dev mode. In oidc mode the route does not exist at
		// all, rather than existing and refusing — an endpoint that mints tokens
		// for any roll number should not be one config flag away from serving.
		if d.Config.DevAuthEnabled() {
			r.Post("/dev/login", auth.DevLogin)
			log.Warn("dev login enabled", "route", "POST /api/v1/dev/login")
		}

		// The live stream (§9), in its own group for one reason: bearerFromQuery
		// has to run BEFORE auth, and the group below applies auth to everything
		// registered inside it. EventSource cannot set an Authorization header,
		// so without the shim the documented client could not authenticate at
		// all. Scoped here so no other route accepts a token in its URL.
		//
		// Not shed, not idempotency-gated, not timed out: it is a read, it holds
		// no database connection between events, and a write timeout on a
		// connection designed to last hours would close it on the first one.
		r.Group(func(r chi.Router) {
			r.Use(bearerFromQuery)
			r.Use(auth.Middleware)
			r.Use(limiter.ByUser(d.Config.RateLimitUserPerMin))

			r.Get("/stream", sse.Stream)
		})

		r.Group(func(r chi.Router) {
			r.Use(auth.Middleware)
			r.Use(limiter.ByUser(d.Config.RateLimitUserPerMin))

			r.Get("/facilities", h.ListFacilities)
			r.Get("/facilities/{id}/availability", h.FacilityAvailability)
			r.Get("/availability", h.CampusAvailability)
			r.Get("/bookings/me", h.ListMyBookings)
			r.Delete("/bookings/{id}", h.CancelBooking)

			// The waitlist half of §6.2/§6.3. None of these is shed: they are
			// not the contended path, and a student accepting a court that is
			// already reserved for them must not be turned away by a queue
			// bound that exists to protect the 6 PM rush.
			r.Post("/bookings/{id}/claim", h.ClaimBooking)
			r.Post("/waitlist", h.JoinWaitlist)
			r.Delete("/waitlist/{id}", h.LeaveWaitlist)

			// Check-in (§7). Neither is shed: a student standing at the door
			// with ninety seconds of token validity left must not be told to
			// come back later because somebody else is booking, and the venue
			// display's poll is a read.
			//
			// The token route is MANAGER-only. It hands out a bearer proof of
			// being at the venue, so serving it to students would let anybody
			// check in from anywhere and the no-show numbers would mean nothing.
			r.With(RequireRole(RoleManager)).
				Get("/facilities/{id}/checkin-token", checkinHandlers.FacilityToken)
			r.Post("/bookings/{id}/check-in", checkinHandlers.CheckIn)

			// Manager closures (§10.4). MANAGER-only, all three: the list is the
			// console's board and it names the students a closure affects, which
			// is not a student's business.
			//
			// Not shed. A closure is a rare administrative write, and the queue
			// bound exists to protect the 6 PM booking rush from itself — turning
			// a manager away from closing a flooded court because students are
			// busy booking it would be exactly backwards.
			r.Route("/closures", func(r chi.Router) {
				r.Use(RequireRole(RoleManager))

				r.With(RequireIdempotencyKey).Post("/", h.CreateClosure)
				r.Get("/", h.ListClosures)
				r.Delete("/{id}", h.ReopenClosure)
			})

			// The race console (§13). Registered ONLY in dev mode, the same
			// rule POST /api/v1/dev/login follows and for the same reason:
			// /demo/reset cancels whatever is standing in the demo slot, which
			// is exactly right on a laptop and unacceptable against a live
			// deployment. An endpoint like that should not be one config flag
			// away from serving — in oidc mode the route does not exist.
			//
			// Neither route is shed, rate-shaped or idempotency-gated. The
			// contention this demonstrates happens INSIDE the handler, between
			// n goroutines calling the booking service directly; putting the
			// shedder in front of the one request that starts them would mean
			// measuring admission control and calling it a constraint.
			if d.Config.DevAuthEnabled() {
				r.Post("/demo/race", demoHandlers.Race)
				r.Post("/demo/reset", demoHandlers.Reset)
			}

			// The write path, and the only shed route.
			r.With(
				RequireIdempotencyKey,
				shedder.ShedMiddleware,
				timeout(d.Config.WriteTimeout),
			).Post("/bookings", h.CreateBooking)
		})
	})

	return r
}

// readyz reports whether this replica can serve traffic.
//
// Redis being down does NOT make the replica unready. Rate limiting fails open
// and nothing on the booking path is authoritative in Redis, so a Redis outage
// degrades the service rather than breaking it — pulling every replica out of
// rotation for it would turn a degradation into an outage.
func readyz(db *store.DB, rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := db.Health(ctx); err != nil {
			JSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unready",
				"reason": "database unreachable",
			})
			return
		}

		body := map[string]string{"status": "ready", "redis": "ok"}
		if rdb != nil {
			if err := rdb.Ping(ctx).Err(); err != nil {
				body["redis"] = "degraded"
			}
		} else {
			body["redis"] = "disabled"
		}
		JSON(w, http.StatusOK, body)
	}
}

// timeout bounds an admitted request. Applied after Shed, so the bound covers
// the work rather than the wait for a slot.
func timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type displayLocCtx struct{}

// DisplayLocation is the campus timezone for THIS request, defaulting to UTC.
//
// Error is a package-level function by design — one envelope, written in one
// place — so it has no receiver to hang configuration on, and the one thing it
// must localise is an alternative's start time. Carrying the zone on the request
// context keeps that out of a package-level variable: two servers in one process
// (which the tests do run) would otherwise share, and race on, one setting.
func DisplayLocation(ctx context.Context) *time.Location {
	if loc, ok := ctx.Value(displayLocCtx{}).(*time.Location); ok && loc != nil {
		return loc
	}
	return time.UTC
}

// withDisplayLocation publishes the campus timezone to the error renderer.
func withDisplayLocation(loc *time.Location) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), displayLocCtx{}, loc)))
		})
	}
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, "+HeaderIdempotencyKey)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
