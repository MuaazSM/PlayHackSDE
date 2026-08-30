// Package config holds the one Config struct, populated once at boot.
//
// Nothing below cmd/ calls os.Getenv. If a package needs a value it takes it as
// a parameter or a struct field — IMPLEMENTATION.md §2.2.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

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
	WriteQueueDepth int
	WriteTimeout    time.Duration

	// Domain windows
	GracePeriod  time.Duration // no-show window
	PromotionTTL time.Duration // waitlist claim expiry

	// Presentation
	TZDisplay string

	// HTTP
	HTTPAddr        string
	ShutdownTimeout time.Duration
}

// Load reads the environment into a Config and validates it. It is the only
// place in the codebase that reads environment variables.
func Load() (*Config, error) {
	c := &Config{
		DBURL:             env("DB_URL", "postgres://playhack:playhack@localhost:6432/playhack?sslmode=disable"),
		DBReplicaURL:      env("DB_REPLICA_URL", ""),
		RedisURL:          env("REDIS_URL", "redis://localhost:6379"),
		AuthMode:          AuthMode(strings.ToLower(env("AUTH_MODE", "dev"))),
		JWTSecret:         env("JWT_SECRET", "dev-secret-not-for-production"),
		OIDCIssuer:        env("OIDC_ISSUER", ""),
		OIDCClientID:      env("OIDC_CLIENT_ID", ""),
		OIDCClientSecret:  env("OIDC_CLIENT_SECRET", ""),
		CheckinHMACSecret: env("CHECKIN_HMAC_SECRET", "dev-checkin-secret"),
		TZDisplay:         env("TZ_DISPLAY", "Asia/Kolkata"),
		HTTPAddr:          env("HTTP_ADDR", ":8080"),
	}

	var err error
	if c.DBMaxConns, err = envInt32("DB_MAX_CONNS", 25); err != nil {
		return nil, err
	}
	if c.WriteQueueDepth, err = envInt("WRITE_QUEUE_DEPTH", 64); err != nil {
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
	case AuthModeDev:
	default:
		return fmt.Errorf("config: AUTH_MODE must be %q or %q, got %q", AuthModeOIDC, AuthModeDev, c.AuthMode)
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
	return nil
}

// DevAuthEnabled reports whether POST /api/v1/dev/login should be mounted.
func (c *Config) DevAuthEnabled() bool { return c.AuthMode == AuthModeDev }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
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
