package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func validConfig() *Config {
	return &Config{
		DBURL: "postgres://localhost/playhack", DBMaxConns: 1,
		AuthMode: AuthModeOIDC, OIDCIssuer: "https://issuer.example",
		OIDCClientID: "sportsbook", TZDisplay: "UTC", NotifierKind: "log",
		WriteQueueDepth: 1,
	}
}

func TestValidateRejectsDevelopmentSecretsOutsideDev(t *testing.T) {
	for _, secret := range []string{"dev-secret-not-for-production", "dev-checkin-secret"} {
		c := validConfig()
		c.JWTSecret = secret
		err := c.validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "development-only")
	}
}

func TestValidateRejectsInvalidTrustedProxyCIDR(t *testing.T) {
	c := validConfig()
	c.TrustedProxyCIDRs = []string{"not-a-cidr"}
	err := c.validate()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS"))
}

func TestLoadDoesNotPopulateDevelopmentSecretsInOIDC(t *testing.T) {
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "https://issuer.example")
	t.Setenv("OIDC_CLIENT_ID", "sportsbook")
	t.Setenv("JWT_SECRET", "")
	t.Setenv("CHECKIN_HMAC_SECRET", "")

	c, err := Load()
	require.NoError(t, err)
	require.Empty(t, c.JWTSecret)
	require.Empty(t, c.CheckinHMACSecret)
}
