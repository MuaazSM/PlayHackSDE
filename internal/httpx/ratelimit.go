package httpx

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
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
	rdb            *redis.Client
	window         time.Duration
	log            *slog.Logger
	trustedProxies []netip.Prefix
}

// NewRateLimiter builds a limiter. A nil client disables limiting entirely,
// which is the same behaviour as Redis being unreachable.
func NewRateLimiter(rdb *redis.Client, window time.Duration, log *slog.Logger, trustedProxyCIDRs ...[]string) *RateLimiter {
	if window <= 0 {
		window = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	var trusted []netip.Prefix
	if len(trustedProxyCIDRs) > 0 {
		for _, cidr := range trustedProxyCIDRs[0] {
			if prefix, err := netip.ParsePrefix(cidr); err == nil {
				trusted = append(trusted, prefix.Masked())
			}
		}
	}
	return &RateLimiter{rdb: rdb, window: window, log: log, trustedProxies: trusted}
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
			if !rl.allow(r.Context(), "ip:"+clientIPWithTrustedProxies(r, rl.trustedProxies), limit) {
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

// clientIP returns the peer address unless that peer is an explicitly trusted
// reverse proxy. Forwarded headers from an untrusted client are ignored, which
// prevents a caller from rotating through arbitrary spoofed rate-limit keys.
func clientIPWithTrustedProxies(r *http.Request, trusted []netip.Prefix) string {
	peer := remoteHost(r.RemoteAddr)
	peerAddr, peerErr := netip.ParseAddr(peer)
	if peerErr != nil || !isTrustedProxy(peerAddr, trusted) {
		if peer == "" {
			return "unknown"
		}
		return peer
	}

	// Walk the proxy chain from the closest hop backwards and select the first
	// address not in the trusted set. A malformed element is treated as the
	// client address, never skipped to broaden trust.
	for _, forwarded := range forwardedFor(r) {
		addr, err := netip.ParseAddr(strings.TrimSpace(forwarded))
		if err != nil || !isTrustedProxy(addr, trusted) {
			if addr.IsValid() {
				return addr.String()
			}
			return peer
		}
	}
	return peer
}

func remoteHost(remote string) string {
	if remote == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	// httptest requests and a few server adapters expose a bare address.
	if addr, err := netip.ParseAddr(remote); err == nil {
		return addr.String()
	}
	return remote
}

func isTrustedProxy(addr netip.Addr, trusted []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func forwardedFor(r *http.Request) []string {
	// X-Forwarded-For is the conventional header and supports a comma-separated
	// chain. Do not accept Forwarded's free-form syntax here; deployments can
	// normalize it at the trusted proxy before reaching the application.
	value := r.Header.Get("X-Forwarded-For")
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	// Header order is client, proxy1, ..., immediate peer. Reverse it so a
	// trusted chain cannot be extended by an untrusted leftmost value.
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return parts
}
