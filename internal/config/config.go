// Package config holds the one Config struct, populated once at boot.
//
// Nothing below cmd/ calls os.Getenv. If a package needs a value it takes it as
// a parameter or a struct field — IMPLEMENTATION.md §2.2.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultWriteQueueDepth bounds concurrent booking writes (WRITE_QUEUE_DEPTH).
//
// This is a TUNED value, not a constant of nature. It trades how many users get
// a definitive answer against how fast that answer arrives: every admitted
// request serialises behind the per-facility advisory lock, so conflict latency
// grows roughly linearly with depth while shed requests stay free.
//
// The end-to-end compose measurements in the Makefile select 24. Keeping a
// different code default made direct `go run ./cmd/api` silently use the known
// slow 128-depth profile and invalidated the rejection-latency guarantee unless
// callers happened to start the service through Make.
//
// This remains environment-overridable, but the safe default is the profile
// with measured margin rather than the largest value observed in a faster test
// container.
const DefaultWriteQueueDepth = 24

// AuthMode selects between real OIDC and the dev login shortcut.
type AuthMode string

const (
	AuthModeOIDC AuthMode = "oidc"
	AuthModeDev  AuthMode = "dev"
)

// Config is the complete runtime configuration. Read once, in cmd/, then passed
// down explicitly.
type Config struct {
	// Database
	DBURL        string
	DBReplicaURL string // falls back to DBURL
	DBMaxConns   int32

	// DBListenURL is a DIRECT Postgres connection string for the outbox
	// dispatcher's LISTEN session — deliberately separate from DBURL, which
	// points at PgBouncer.
	//
	// LISTEN is session state. Transaction-mode pooling hands the backend to
	// another client the moment the transaction ends, so a subscription taken
	// through the pooler is dropped without anybody being told. Falls back to
	// DBURL, which is correct for the demo (no pooler in front of it) and wrong
	// in front of PgBouncer — set it explicitly there.
	DBListenURL string

	// Cache / bus
	RedisURL string

	// Auth
	AuthMode         AuthMode
	JWTSecret        string
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string

	// Check-in QR minting
	CheckinHMACSecret string

	// Load shedding — §4.6. Losing must be faster than winning.
	// See DefaultWriteQueueDepth: tuned per environment, not a fixed constant.
	WriteQueueDepth int
	WriteTimeout    time.Duration

	// Domain windows
	GracePeriod  time.Duration // no-show window
	PromotionTTL time.Duration // waitlist claim expiry

	// Rate limits, per minute. Not correctness controls — the limiter fails
	// open — so these are generous enough to be invisible to a real student and
	// tight enough to make a script expensive.
	RateLimitIPPerMin   int
	RateLimitUserPerMin int

	// Side effects — §8. Nothing here affects correctness: the outbox rows are
	// written either way, and these only decide who eventually reads them.
	//
	// EmbedWorkers runs the dispatcher and the sweepers inside the api binary.
	// Default true, because the demo is one process and one process cannot lose
	// half of itself on stage. Set false when running cmd/worker separately.
	EmbedWorkers bool

	// NotifierKind selects the transport: "log" (demo default), "webpush",
	// "email".
	NotifierKind string

	// Web Push identity (RFC 8292). Pinned, not generated at boot — rotating the
	// keypair invalidates every existing browser subscription.
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	VAPIDSubject    string

	// EmailFrom is the envelope sender for the email fallback.
	EmailFrom string

	// Presentation
	TZDisplay string

	// HTTP
	HTTPAddr        string
	ShutdownTimeout time.Duration

	// TrustedProxyCIDRs identifies reverse proxies that are allowed to supply
	// X-Forwarded-For. An empty list is intentionally the safe default: direct
	// clients cannot spoof the address used by the IP rate limiter.
	TrustedProxyCIDRs []string
}

// Load reads the environment into a Config and validates it. It is the only
// place in the codebase that reads environment variables.
func Load() (*Config, error) {
	authMode := AuthMode(strings.ToLower(env("AUTH_MODE", "dev")))
	jwtSecret := env("JWT_SECRET", "")
	checkinSecret := env("CHECKIN_HMAC_SECRET", "")
	// Development is the only mode in which predictable local-only secrets are
	// acceptable. Production/OIDC must never silently inherit them.
	if authMode == AuthModeDev {
		if jwtSecret == "" {
			jwtSecret = "dev-secret-not-for-production"
		}
		if checkinSecret == "" {
			checkinSecret = "dev-checkin-secret"
		}
	}
	c := &Config{
		DBURL:             env("DB_URL", "postgres://playhack:playhack@localhost:6432/playhack?sslmode=disable"),
		DBReplicaURL:      env("DB_REPLICA_URL", ""),
		DBListenURL:       env("DB_LISTEN_URL", ""),
		NotifierKind:      strings.ToLower(env("NOTIFIER", "log")),
		VAPIDPublicKey:    env("VAPID_PUBLIC_KEY", ""),
		VAPIDPrivateKey:   env("VAPID_PRIVATE_KEY", ""),
		VAPIDSubject:      env("VAPID_SUBJECT", ""),
		EmailFrom:         env("EMAIL_FROM", ""),
		RedisURL:          env("REDIS_URL", "redis://localhost:6379"),
		AuthMode:          authMode,
		JWTSecret:         jwtSecret,
		OIDCIssuer:        env("OIDC_ISSUER", ""),
		OIDCClientID:      env("OIDC_CLIENT_ID", ""),
		OIDCClientSecret:  env("OIDC_CLIENT_SECRET", ""),
		CheckinHMACSecret: checkinSecret,
		TZDisplay:         env("TZ_DISPLAY", "Asia/Kolkata"),
		HTTPAddr:          env("HTTP_ADDR", ":8080"),
		TrustedProxyCIDRs: envCSV("TRUSTED_PROXY_CIDRS"),
	}

	var err error
	if c.DBMaxConns, err = envInt32("DB_MAX_CONNS", 25); err != nil {
		return nil, err
	}
	if c.WriteQueueDepth, err = envInt("WRITE_QUEUE_DEPTH", DefaultWriteQueueDepth); err != nil {
		return nil, err
	}
	if c.WriteTimeout, err = envMillis("WRITE_TIMEOUT_MS", 800); err != nil {
		return nil, err
	}
	if c.GracePeriod, err = envMinutes("GRACE_PERIOD_MIN", 15); err != nil {
		return nil, err
	}
	if c.PromotionTTL, err = envMinutes("PROMOTION_TTL_MIN", 10); err != nil {
		return nil, err
	}
	if c.ShutdownTimeout, err = envSeconds("SHUTDOWN_TIMEOUT_SEC", 15); err != nil {
		return nil, err
	}
	if c.RateLimitIPPerMin, err = envInt("RATE_LIMIT_IP_PER_MIN", 600); err != nil {
		return nil, err
	}
	if c.RateLimitUserPerMin, err = envInt("RATE_LIMIT_USER_PER_MIN", 120); err != nil {
		return nil, err
	}
	if c.EmbedWorkers, err = envBool("EMBED_WORKERS", true); err != nil {
		return nil, err
	}

	// The dispatcher's LISTEN session defaults to the main URL. Correct for the
	// demo, where nothing sits in front of Postgres; set DB_LISTEN_URL to the
	// direct port when PgBouncer does.
	if c.DBListenURL == "" {
		c.DBListenURL = c.DBURL
	}

	// DB_REPLICA_URL is optional; availability reads fall back to the primary.
	// The architecture claim survives the fallback, the demo does not depend on
	// the split existing — §2.1.
	if c.DBReplicaURL == "" {
		c.DBReplicaURL = c.DBURL
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.DBURL == "" {
		return fmt.Errorf("config: DB_URL is required")
	}
	switch c.AuthMode {
	case AuthModeOIDC:
		if c.OIDCIssuer == "" || c.OIDCClientID == "" {
			return fmt.Errorf("config: AUTH_MODE=oidc requires OIDC_ISSUER and OIDC_CLIENT_ID")
		}
		if err := validateOIDCIssuer(c.OIDCIssuer); err != nil {
			return err
		}
	case AuthModeDev:
		if strings.TrimSpace(c.JWTSecret) == "" {
			return fmt.Errorf("config: AUTH_MODE=dev requires JWT_SECRET")
		}
	default:
		return fmt.Errorf("config: AUTH_MODE must be %q or %q, got %q", AuthModeOIDC, AuthModeDev, c.AuthMode)
	}
	if c.AuthMode != AuthModeDev {
		for name, value := range map[string]string{
			"JWT_SECRET":          c.JWTSecret,
			"CHECKIN_HMAC_SECRET": c.CheckinHMACSecret,
		} {
			if value == "dev-secret-not-for-production" || value == "dev-checkin-secret" {
				return fmt.Errorf("config: %s contains a development-only default outside AUTH_MODE=dev", name)
			}
		}
	}
	if c.DBMaxConns < 1 {
		return fmt.Errorf("config: DB_MAX_CONNS must be >= 1, got %d", c.DBMaxConns)
	}
	if c.WriteQueueDepth < 1 {
		return fmt.Errorf("config: WRITE_QUEUE_DEPTH must be >= 1, got %d", c.WriteQueueDepth)
	}
	if _, err := time.LoadLocation(c.TZDisplay); err != nil {
		return fmt.Errorf("config: TZ_DISPLAY %q is not a known location: %w", c.TZDisplay, err)
	}
	for _, cidr := range c.TrustedProxyCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("config: TRUSTED_PROXY_CIDRS contains invalid CIDR %q: %w", cidr, err)
		}
	}
	switch c.NotifierKind {
	case "log", "webpush", "email":
	default:
		return fmt.Errorf("config: NOTIFIER must be log, webpush or email, got %q", c.NotifierKind)
	}
	return nil
}

func validateOIDCIssuer(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("config: OIDC_ISSUER must be an absolute HTTPS URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	// Plain HTTP is useful only for a loopback provider in local tests. Never
	// allow an Internet or LAN identity provider to downgrade token transport.
	if u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()) {
		return nil
	}
	return fmt.Errorf("config: OIDC_ISSUER must use HTTPS (HTTP is allowed only for loopback)")
}

// DevAuthEnabled reports whether POST /api/v1/dev/login should be mounted.
func (c *Config) DevAuthEnabled() bool { return c.AuthMode == AuthModeDev }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envCSV(key string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config: %s must be a boolean: %w", key, err)
	}
	return b, nil
}

func envInt32(key string, def int32) (int32, error) {
	n, err := envInt(key, int(def))
	if err != nil {
		return 0, err
	}
	return int32(n), nil
}

func envMillis(key string, def int) (time.Duration, error) {
	n, err := envInt(key, def)
	if err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Millisecond, nil
}

func envMinutes(key string, def int) (time.Duration, error) {
	n, err := envInt(key, def)
	if err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Minute, nil
}

func envSeconds(key string, def int) (time.Duration, error) {
	n, err := envInt(key, def)
	if err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Second, nil
}
