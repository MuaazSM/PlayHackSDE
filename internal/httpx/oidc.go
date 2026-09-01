package httpx

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	oidcDiscoveryTimeout = 5 * time.Second
	maxOIDCMetadataBytes = 1 << 20
)

var oidcSigningAlgorithms = []string{
	jwt.SigningMethodRS256.Alg(), jwt.SigningMethodRS384.Alg(), jwt.SigningMethodRS512.Alg(),
	jwt.SigningMethodES256.Alg(), jwt.SigningMethodES384.Alg(), jwt.SigningMethodES512.Alg(),
	jwt.SigningMethodEdDSA.Alg(),
}

type oidcDiscovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

type oidcJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type oidcJWKS struct {
	Keys []oidcJWK `json:"keys"`
}

type oidcKey struct {
	key any
	alg string
}

// oidcVerifier performs standards-based issuer discovery and signature
// validation. Metadata and keys are cached, and an unknown key id triggers one
// refresh so normal provider key rotation does not require an API restart.
type oidcVerifier struct {
	issuer   string
	clientID string
	client   *http.Client

	mu        sync.RWMutex
	discovery oidcDiscovery
	keys      map[string]oidcKey
}

func newOIDCVerifier(issuer, clientID string, client *http.Client) *oidcVerifier {
	if client == nil {
		client = &http.Client{
			Timeout: oidcDiscoveryTimeout,
			// Discovery and JWKS are security-sensitive. Do not silently follow
			// a provider redirect to a different host or down to HTTP.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &oidcVerifier{
		issuer:   strings.TrimSpace(issuer),
		clientID: clientID,
		client:   client,
	}
}

func (v *oidcVerifier) verify(ctx context.Context, raw string) (map[string]any, error) {
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		return v.key(ctx, token)
	}, jwt.WithValidMethods(oidcSigningAlgorithms),
		jwt.WithIssuer(v.issuer), jwt.WithAudience(v.clientID), jwt.WithLeeway(time.Minute))
	if err != nil {
		return nil, err
	}
	if parsed == nil || !parsed.Valid {
		return nil, errors.New("OIDC token is invalid")
	}
	// An OIDC access token used for this API must be time-bounded. jwt's claims
	// validator accepts a missing exp for compatibility with generic JWTs.
	if _, ok := claims["exp"]; !ok {
		return nil, errors.New("OIDC token has no exp claim")
	}
	if sub, ok := claims["sub"].(string); !ok || strings.TrimSpace(sub) == "" {
		return nil, errors.New("OIDC token has no subject")
	}
	return claims, nil
}

func (v *oidcVerifier) key(ctx context.Context, token *jwt.Token) (any, error) {
	if token == nil {
		return nil, errors.New("OIDC token is missing")
	}
	if token.Method == nil {
		return nil, errors.New("OIDC token has no signing method")
	}
	if !contains(oidcSigningAlgorithms, token.Method.Alg()) {
		return nil, fmt.Errorf("OIDC signing algorithm %q is not allowed", token.Method.Alg())
	}
	kid, ok := token.Header["kid"].(string)
	if !ok || strings.TrimSpace(kid) == "" {
		return nil, errors.New("OIDC token has no key id")
	}
	if err := v.load(ctx, false); err != nil {
		return nil, err
	}
	v.mu.RLock()
	key, found := v.keys[kid]
	v.mu.RUnlock()
	if !found {
		if err := v.load(ctx, true); err != nil {
			return nil, err
		}
		v.mu.RLock()
		key, found = v.keys[kid]
		v.mu.RUnlock()
	}
	if !found {
		return nil, fmt.Errorf("OIDC signing key %q is not trusted", kid)
	}
	if key.alg != "" && key.alg != token.Method.Alg() {
		return nil, fmt.Errorf("OIDC key %q is not valid for algorithm %q", kid, token.Method.Alg())
	}
	if !oidcKeyMatchesMethod(key.key, token.Method.Alg()) {
		return nil, fmt.Errorf("OIDC key %q has an incompatible key type", kid)
	}
	return key.key, nil
}

func oidcKeyMatchesMethod(key any, alg string) bool {
	switch alg {
	case "RS256", "RS384", "RS512":
		_, ok := key.(*rsa.PublicKey)
		return ok
	case "ES256", "ES384", "ES512":
		publicKey, ok := key.(*ecdsa.PublicKey)
		if !ok || publicKey.Curve == nil {
			return false
		}
		want := map[string]string{"ES256": "P-256", "ES384": "P-384", "ES512": "P-521"}[alg]
		return publicKey.Curve.Params().Name == want
	case "EdDSA":
		_, ok := key.(ed25519.PublicKey)
		return ok
	default:
		return false
	}
}

func (v *oidcVerifier) load(ctx context.Context, force bool) error {
	v.mu.RLock()
	loaded := v.discovery.JWKSURI != "" && len(v.keys) > 0
	v.mu.RUnlock()
	if loaded && !force {
		return nil
	}

	requestCtx, cancel := context.WithTimeout(ctx, oidcDiscoveryTimeout)
	defer cancel()
	discovery := oidcDiscovery{}
	if !loaded || force {
		body, err := v.getJSON(requestCtx, strings.TrimRight(v.issuer, "/")+"/.well-known/openid-configuration")
		if err != nil {
			return fmt.Errorf("OIDC discovery failed: %w", err)
		}
		if err := json.Unmarshal(body, &discovery); err != nil {
			return fmt.Errorf("OIDC discovery response is invalid: %w", err)
		}
		if strings.TrimSpace(discovery.Issuer) != v.issuer {
			return fmt.Errorf("OIDC discovery issuer %q does not match configured issuer", discovery.Issuer)
		}
		if discovery.JWKSURI == "" {
			return errors.New("OIDC discovery response has no jwks_uri")
		}
	} else {
		v.mu.RLock()
		discovery = v.discovery
		v.mu.RUnlock()
	}

	body, err := v.getJSON(requestCtx, discovery.JWKSURI)
	if err != nil {
		return fmt.Errorf("OIDC JWKS fetch failed: %w", err)
	}
	var document oidcJWKS
	if err := json.Unmarshal(body, &document); err != nil {
		return fmt.Errorf("OIDC JWKS response is invalid: %w", err)
	}
	keys := make(map[string]oidcKey, len(document.Keys))
	for _, jwk := range document.Keys {
		if jwk.Kid == "" || (jwk.Use != "" && jwk.Use != "sig") {
			continue
		}
		key, err := parseOIDCJWK(jwk)
		if err != nil {
			// Providers may publish encryption keys alongside signing keys. Ignore
			// malformed/unsupported entries, but require at least one usable key.
			continue
		}
		keys[jwk.Kid] = oidcKey{key: key, alg: jwk.Alg}
	}
	if len(keys) == 0 {
		return errors.New("OIDC JWKS contains no usable signing keys")
	}
	v.mu.Lock()
	v.discovery = discovery
	v.keys = keys
	v.mu.Unlock()
	return nil
}

func (v *oidcVerifier) getJSON(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOIDCMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxOIDCMetadataBytes {
		return nil, errors.New("provider response is too large")
	}
	return body, nil
}

func parseOIDCJWK(jwk oidcJWK) (any, error) {
	if jwk.Alg != "" && !contains(oidcSigningAlgorithms, jwk.Alg) {
		return nil, errors.New("unsupported signing algorithm")
	}
	decode := func(value string) ([]byte, error) {
		if value == "" {
			return nil, errors.New("missing key parameter")
		}
		return base64.RawURLEncoding.DecodeString(value)
	}
	switch jwk.Kty {
	case "RSA":
		n, err := decode(jwk.N)
		if err != nil {
			return nil, err
		}
		eBytes, err := decode(jwk.E)
		if err != nil {
			return nil, err
		}
		if len(n) < 256 || len(eBytes) > 8 {
			return nil, errors.New("RSA key size is invalid")
		}
		e := new(big.Int).SetBytes(eBytes)
		if !e.IsInt64() || e.Int64() < 3 || e.Int64()%2 == 0 {
			return nil, errors.New("RSA exponent is invalid")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch jwk.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, errors.New("unsupported EC curve")
		}
		xBytes, err := decode(jwk.X)
		if err != nil {
			return nil, err
		}
		yBytes, err := decode(jwk.Y)
		if err != nil {
			return nil, err
		}
		// JWK coordinates are minimal big-endian; SEC 1 uncompressed points are
		// fixed-width. ParseUncompressedPublicKey validates the point is on the
		// curve, which replaces a hand-rolled IsOnCurve check.
		size := (curve.Params().BitSize + 7) / 8
		if len(xBytes) > size || len(yBytes) > size {
			return nil, errors.New("EC coordinate is too large for the curve")
		}
		point := make([]byte, 1+2*size)
		point[0] = 0x04
		copy(point[1+size-len(xBytes):1+size], xBytes)
		copy(point[1+2*size-len(yBytes):1+2*size], yBytes)
		return ecdsa.ParseUncompressedPublicKey(curve, point)
	case "OKP":
		if jwk.Crv != "Ed25519" {
			return nil, errors.New("unsupported OKP curve")
		}
		x, err := decode(jwk.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("Ed25519 key is invalid")
		}
		return ed25519.PublicKey(x), nil
	default:
		return nil, errors.New("unsupported key type")
	}
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
