package sso

// Red-team regressions for the OIDC id_token verification surface (design §7:
// "id_token 验签+iss+aud+exp+nonce"). Each test constructs a hostile token/response
// and asserts ExchangeAndVerify REFUSES it. They complement oidc_test.go's
// BadSignature/WrongAudience/WrongIssuer/Expired/NonceMismatch coverage with the
// forgery variants the red-team brief calls out: alg=none, JWKS/kid confusion,
// and an aud array that omits the client_id.
//
// Run with NO_PROXY=127.0.0.1,localhost so the loopback mock IdP is never proxied.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// TestRedteam_AlgNoneRejected — an id_token with header {"alg":"none"} and no
// signature must be refused. This is the classic "algorithm confusion → forge any
// identity" attack: if the verifier honored alg=none, an attacker who never held a
// signing key could mint an arbitrary sub/email. go-oidc's verifier only accepts
// the discovery-advertised signing algs (RS256 here), so "none" is rejected.
//
// A standalone IdP is used (not newMockIdP) so the issuer in the token matches the
// discovery doc exactly — the ONLY reason the exchange can fail here is the
// algorithm, making the assertion unambiguous.
func TestRedteam_AlgNoneRejected(t *testing.T) {
	mux := http.NewServeMux()
	var issuer string
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/authorize",
			"token_endpoint":                        issuer + "/token",
			"jwks_uri":                              issuer + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		forged := unsignedJWT(map[string]any{
			"iss":            issuer,
			"sub":            "attacker-sub",
			"aud":            "test-client",
			"exp":            time.Now().Add(time.Hour).Unix(),
			"iat":            time.Now().Add(-time.Minute).Unix(),
			"nonce":          "n",
			"email":          "admin@example.com",
			"email_verified": true,
		})
		writeJSON(w, map[string]any{
			"access_token": "a", "token_type": "Bearer", "expires_in": 3600, "id_token": forged,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	issuer = srv.URL

	p, err := NewOIDCProvider(OIDCConfig{
		Issuer:      issuer,
		ClientID:    "test-client",
		RedirectURL: "https://app.example.com/cb",
	}, WithHTTPClient(noProxyClient()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n"); err == nil {
		t.Fatal("an alg=none id_token must be rejected (algorithm-confusion forgery)")
	}
}

// TestRedteam_KidConfusionRejected — an attacker signs the token with a key they
// generated but sets the JWS `kid` header to the IdP's advertised key id, hoping
// the verifier trusts the kid label rather than the actual key material. The
// verifier fetches the REAL public key for that kid from the IdP's JWKS and the
// attacker's signature fails against it.
func TestRedteam_KidConfusionRejected(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "n"
	attackerKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	// Sign with the attacker key while JWKS still advertises the honest key under
	// the same kid (idp.keyID is left unchanged in signJWT's header).
	idp.signKey = attackerKey

	p := idp.newProvider(t, nil)
	if _, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n"); err == nil {
		t.Fatal("a token signed by an attacker key but labeled with the IdP kid must be rejected")
	}
}

// TestRedteam_AudArrayWithoutClientIDRejected — id_token `aud` is a JSON array
// that does NOT contain our client_id (a token minted for a different RP). Even
// with a valid signature/iss/exp/nonce it must be refused, else a token issued for
// another relying party could be replayed against us.
func TestRedteam_AudArrayWithoutClientIDRejected(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "n"
	idp.extraClaims = map[string]any{"aud": []string{"some-other-rp", "yet-another"}}

	p := idp.newProvider(t, nil)
	if _, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n"); err == nil {
		t.Fatal("an id_token whose aud array omits our client_id must be rejected")
	}
}

// TestRedteam_NoNonceInTokenRejected — the token carries NO nonce claim while the
// flow expects one. A missing nonce must fail the explicit nonce check (id_token
// replay defense), not be treated as "matches empty".
func TestRedteam_NoNonceInTokenRejected(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "" // token minted with an empty/absent nonce
	p := idp.newProvider(t, nil)

	if _, err := p.ExchangeAndVerify(context.Background(), "c", "v", "expected-nonce"); err == nil {
		t.Fatal("a token with no nonce must be rejected when a nonce was expected")
	}
}

// --- helpers ---

// unsignedJWT builds a compact JWT with header {"alg":"none","typ":"JWT"} and an
// empty signature segment, carrying the given claims.
func unsignedJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc(header) + "." + enc(payload) + "."
}
