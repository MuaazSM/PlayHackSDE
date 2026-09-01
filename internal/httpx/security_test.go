package httpx

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func TestClientIPIgnoresUntrustedForwardedHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.99")

	require.Equal(t, "192.0.2.10", clientIPWithTrustedProxies(r, nil))
}

func TestClientIPUsesForwardedChainOnlyFromTrustedProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.2:443"
	r.Header.Set("X-Forwarded-For", "198.51.100.99, 10.0.0.1")

	prefix := netip.MustParsePrefix("10.0.0.0/8")
	require.Equal(t, "198.51.100.99", clientIPWithTrustedProxies(r, []netip.Prefix{prefix}))
}

func TestOIDCVerifierDiscoversAndValidatesJWT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":   "http://" + r.Host,
				"jwks_uri": "http://" + r.Host + "/keys",
			})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "test-key", "use": "sig", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	issuer := strings.TrimRight(server.URL, "/")
	verifier := newOIDCVerifier(issuer, "sportsbook", server.Client())
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": issuer, "aud": "sportsbook", "sub": "provider-user", "roll_no": "student01",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key"
	raw, err := token.SignedString(key)
	require.NoError(t, err)

	claims, err := verifier.verify(context.Background(), raw)
	require.NoError(t, err)
	require.Equal(t, "student01", claims["roll_no"])

	token2 := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": issuer, "aud": "another-client", "sub": "provider-user",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	token2.Header["kid"] = "test-key"
	wrongAudience, err := token2.SignedString(key)
	require.NoError(t, err)
	_, err = verifier.verify(context.Background(), wrongAudience)
	require.Error(t, err)
}
