package httpx

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimiter is a fixed-window counter in Redis: INCR, then EXPIRE on first
// hit. Not a token bucket's smooth drip, but one round trip and no Lua, and at
// this scale the difference is invisible.
//
// IT FAILS OPEN. Rate limiting is not a correctness control — the exclusion
// constraint is — so a Redis outage must never be able to take the booking path
// down. A limiter that fails closed converts a cache outage into an outage,
// which is strictly worse than briefly serving unlimited requests to a campus of
// 8,000 students.
//
// This is also why Redis being wiped mid-demo costs nothing here: the counters
// restart, and correctness never depended on them (non-negotiable #3).
type RateLimiter struct {
	rdb    *redis.Client
	window time.Duration
	log    *slog.Logger
}

// NewRateLimiter builds a limiter. A nil client disables limiting entirely,
// which is the same behaviour as Redis being unreachable.
func NewRateLimiter(rdb *redis.Client, window time.Duration, log *slog.Logger) *RateLimiter {
	if window <= 0 {
		window = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &RateLimiter{rdb: rdb, window: window, log: log}
}

// allow reports whether key is under limit for the current window.
//
// The bool it returns on error is true: see the fail-open note above.
func (rl *RateLimiter) allow(ctx context.Context, key string, limit int) bool {
	if rl.rdb == nil || limit <= 0 {
		return true
	}

	bucket := fmt.Sprintf("rl:%s:%d", key, time.Now().UnixNano()/int64(rl.window))

	n, err := rl.rdb.Incr(ctx, bucket).Result()
	if err != nil {
		rl.log.WarnContext(ctx, "rate limiter unavailable, failing open", "err", err, "key", key)
		return true
	}
	if n == 1 {
		// Only the first hit sets the TTL, so the window does not slide forward
		// on every request and trap a caller indefinitely.
		if err := rl.rdb.Expire(ctx, bucket, rl.window).Err(); err != nil {
			rl.log.WarnContext(ctx, "rate limiter expire failed", "err", err, "key", key)
		}
	}
	return n <= int64(limit)
}

// ByIP limits before authentication.
//
// The split around Auth is deliberate — see router.go. This bucket exists so an
// unauthenticated flood is dropped before it can cost a JWT verification.
func (rl *RateLimiter) ByIP(limit int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !rl.allow(r.Context(), "ip:"+clientIP(r), limit) {
				Error(w, r, fmt.Errorf("%w: too many requests from this address", ErrRateLimited))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ByUser limits after authentication, so one authenticated user cannot exhaust
// the budget for everyone behind a shared campus NAT address.
func (rl *RateLimiter) ByUser(limit int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				// Unauthenticated requests never reach this middleware; if one
				// does, the IP bucket has already covered it.
				next.ServeHTTP(w, r)
				return
			}
			if !rl.allow(r.Context(), "user:"+p.UserID.String(), limit) {
				Error(w, r, fmt.Errorf("%w: too many requests", ErrRateLimited))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP prefers chi's RealIP result, which has already consulted
// X-Forwarded-For.
func clientIP(r *http.Request) string {
	if ip := r.RemoteAddr; ip != "" {
		if host, _, err := splitHostPort(ip); err == nil {
			return host
		}
		return ip
	}
	return "unknown"
}

func splitHostPort(hostport string) (host, port string, err error) {
	for i := len(hostport) - 1; i >= 0; i-- {
		if hostport[i] == ':' {
			return hostport[:i], hostport[i+1:], nil
		}
	}
	return hostport, "", fmt.Errorf("no port")
}
