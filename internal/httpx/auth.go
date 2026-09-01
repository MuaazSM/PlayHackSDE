package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Principal is the authenticated caller.
type Principal struct {
	UserID uuid.UUID
	RollNo string
	Role   string
}

type principalKey struct{}

// PrincipalFrom returns the authenticated caller, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Authenticator verifies bearer tokens. AUTH_MODE=dev signs and verifies with a
// shared secret and enables POST /api/v1/dev/login. AUTH_MODE=oidc performs
// issuer discovery, JWKS signature validation and a local user/role lookup.
type Authenticator struct {
	mode   config.AuthMode
	secret []byte
	pool   *pgxpool.Pool
	ttl    time.Duration
	oidc   *oidcVerifier
}

// NewAuthenticator builds the auth middleware.
func NewAuthenticator(cfg *config.Config, pool *pgxpool.Pool) *Authenticator {
	a := &Authenticator{
		mode:   cfg.AuthMode,
		secret: []byte(cfg.JWTSecret),
		pool:   pool,
		ttl:    12 * time.Hour,
	}
	if cfg.AuthMode == config.AuthModeOIDC {
		a.oidc = newOIDCVerifier(cfg.OIDCIssuer, cfg.OIDCClientID, nil)
	}
	return a
}

// NewAuthenticatorWithHTTPClient is useful for tests and for applications that
// already own an HTTP transport. Production callers should normally use
// NewAuthenticator, which uses a bounded client for OIDC discovery/JWKS.
func NewAuthenticatorWithHTTPClient(cfg *config.Config, pool *pgxpool.Pool, client *http.Client) *Authenticator {
	a := NewAuthenticator(cfg, pool)
	if cfg.AuthMode == config.AuthModeOIDC {
		a.oidc = newOIDCVerifier(cfg.OIDCIssuer, cfg.OIDCClientID, client)
	}
	return a
}

type claims struct {
	RollNo string `json:"roll_no"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

// Middleware rejects anything without a valid bearer token.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearer(r)
		if raw == "" {
			Error(w, r, fmt.Errorf("%w: no bearer token", ErrUnauthenticated))
			return
		}

		p, err := a.verifyContext(r.Context(), raw)
		if err != nil {
			Error(w, r, fmt.Errorf("%w: %v", ErrUnauthenticated, err))
			return
		}

		ctx := context.WithValue(r.Context(), principalKey{}, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func (a *Authenticator) verifyContext(ctx context.Context, raw string) (Principal, error) {
	if a.mode == config.AuthModeOIDC {
		return a.verifyOIDC(ctx, raw)
	}
	var c claims

	// Algorithm is pinned. Accepting whatever the token declares is how "alg:
	// none" and HMAC-vs-RSA confusion get in.
	_, err := jwt.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return a.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return Principal{}, err
	}

	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return Principal{}, fmt.Errorf("subject is not a user id: %w", err)
	}

	return Principal{UserID: id, RollNo: c.RollNo, Role: c.Role}, nil
}

func (a *Authenticator) verifyOIDC(ctx context.Context, raw string) (Principal, error) {
	if a.oidc == nil {
		return Principal{}, errors.New("OIDC verifier is not configured")
	}
	claims, err := a.oidc.verify(ctx, raw)
	if err != nil {
		return Principal{}, err
	}
	// Never accept a role (or a user id) from an identity-provider token. The
	// provider proves who the subject is; local database state is authoritative
	// for whether that person may use this application and what they may do.
	rollNo := firstClaimString(claims, "roll_no", "preferred_username", "username")
	if rollNo == "" {
		return Principal{}, errors.New("OIDC token has no supported user claim")
	}
	if a.pool == nil {
		return Principal{}, errors.New("user database is unavailable")
	}
	var p Principal
	if err := a.pool.QueryRow(ctx,
		`SELECT id, roll_no, role::text FROM users WHERE roll_no = $1`, rollNo,
	).Scan(&p.UserID, &p.RollNo, &p.Role); err != nil {
		return Principal{}, errors.New("OIDC user is not registered")
	}
	return p, nil
}

func firstClaimString(claims map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := claims[name].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Sign issues a token. Only reachable through the dev login route.
func (a *Authenticator) Sign(p Principal, now time.Time) (string, error) {
	if a.mode != config.AuthModeDev || len(a.secret) == 0 {
		return "", errors.New("token signing is available only in AUTH_MODE=dev")
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		RollNo: p.RollNo,
		Role:   p.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(a.ttl)),
		},
	})
	return tok.SignedString(a.secret)
}

// DevLogin exchanges a roll number for a signed token.
//
// Registered ONLY when AUTH_MODE=dev — see router.go. It takes no password by
// design: it is a demo shortcut, and pretending otherwise would suggest it is
// something you could ship.
func (a *Authenticator) DevLogin(w http.ResponseWriter, r *http.Request) {
	if a.mode != config.AuthModeDev {
		Error(w, r, fmt.Errorf("%w: dev login is disabled", ErrUnauthenticated))
		return
	}
	var req struct {
		RollNo string `json:"roll_no"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		Error(w, r, fmt.Errorf("%w: body must be JSON", ErrBadRequest))
		return
	}
	if req.RollNo == "" {
		Error(w, r, fmt.Errorf("%w: roll_no is required", ErrBadRequest))
		return
	}

	var p Principal
	err := a.pool.QueryRow(r.Context(),
		`SELECT id, roll_no, role::text FROM users WHERE roll_no = $1`, req.RollNo,
	).Scan(&p.UserID, &p.RollNo, &p.Role)
	if err != nil {
		Error(w, r, fmt.Errorf("%w: no such user", ErrUnauthenticated))
		return
	}

	token, err := a.Sign(p, time.Now())
	if err != nil {
		Error(w, r, err)
		return
	}

	JSON(w, http.StatusOK, map[string]any{
		"token":   token,
		"user_id": p.UserID,
		"roll_no": p.RollNo,
		"role":    p.Role,
	})
}
