package handles

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// ssoMockIdP is a minimal OIDC provider (discovery + JWKS + token) for the
// handles-layer callback tests. It mints an id_token whose sub/email/nonce are
// driven by exported knobs so a test can shape the identity the callback sees.
// PKCE is intentionally not enforced here (the engine's PKCE checks are covered
// by internal/authn/sso); these tests exercise the callback's state single-use,
// identity resolution and landing behavior.
type ssoMockIdP struct {
	server        *httptest.Server
	key           *rsa.PrivateKey
	keyID         string
	clientID      string
	issuer        string
	subject       string
	email         string
	emailVerified bool
	username      string
	nonce         string
	expiry        time.Time
}

func newSSOMockIdP(t *testing.T) *ssoMockIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	idp := &ssoMockIdP{
		key:           key,
		keyID:         "test-key-1",
		clientID:      "test-client",
		subject:       "sub-123",
		email:         "user@idp.example.com",
		emailVerified: true,
		username:      "idp-user",
		expiry:        time.Now().Add(time.Hour),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeMockJSON(w, map[string]any{
			"issuer":                                idp.issuer,
			"authorization_endpoint":                idp.issuer + "/authorize",
			"token_endpoint":                        idp.issuer + "/token",
			"jwks_uri":                              idp.issuer + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeMockJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &idp.key.PublicKey, KeyID: idp.keyID, Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		claims := map[string]any{
			"iss":                idp.issuer,
			"sub":                idp.subject,
			"aud":                idp.clientID,
			"exp":                idp.expiry.Unix(),
			"iat":                time.Now().Add(-time.Minute).Unix(),
			"nonce":              idp.nonce,
			"email":              idp.email,
			"email_verified":     idp.emailVerified,
			"preferred_username": idp.username,
		}
		writeMockJSON(w, map[string]any{
			"access_token": "mock-access",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idp.signJWT(claims),
		})
	})
	idp.server = httptest.NewServer(mux)
	idp.issuer = idp.server.URL
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *ssoMockIdP) signJWT(claims map[string]any) string {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: idp.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", idp.keyID),
	)
	if err != nil {
		panic(err)
	}
	payload, _ := json.Marshal(claims)
	jws, err := signer.Sign(payload)
	if err != nil {
		panic(err)
	}
	s, err := jws.CompactSerialize()
	if err != nil {
		panic(err)
	}
	return s
}

func writeMockJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func noProxyHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{}}
}
