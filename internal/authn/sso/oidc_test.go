package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// mockIdP is a self-contained OIDC identity provider backed by httptest: it
// serves discovery + JWKS (with a self-signed RSA key) and a token endpoint that
// mints id_tokens whose claims/signing are driven by the exported knobs below.
// Tests flip a knob (wrong aud, wrong iss, expired, wrong nonce, wrong signing
// key, ...) then run ExchangeAndVerify to assert the engine's checks reject it.
type mockIdP struct {
	server   *httptest.Server
	key      *rsa.PrivateKey // the key advertised in JWKS (the "trusted" key)
	keyID    string
	clientID string

	issuer string // == server.URL; the discovery doc's issuer field

	// token-minting knobs, read at request time so tests can set them after
	// construction but before ExchangeAndVerify.
	signKey        *rsa.PrivateKey // key actually used to sign (default: key)
	issuerOverride string          // non-empty overrides the id_token iss claim
	audience       string          // id_token aud (default: clientID)
	subject        string
	nonce          string
	email          string
	emailVerified  bool
	username       string
	name           string
	expiry         time.Time
	extraClaims    map[string]any
	omitIDToken    bool

	// expectedChallenge, if set, makes the token endpoint enforce PKCE:
	// S256(code_verifier) must equal it, else the exchange 400s.
	expectedChallenge string

	discoveryHits int64
	jwksHits      int64
}

func newMockIdP(t *testing.T) *mockIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	idp := &mockIdP{
		key:           key,
		signKey:       key,
		keyID:         "test-key-1",
		clientID:      "test-client",
		audience:      "test-client",
		subject:       "sub-abc-123",
		email:         "alice@example.com",
		emailVerified: true,
		username:      "alice",
		name:          "Alice Example",
		expiry:        time.Now().Add(time.Hour),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.handleDiscovery)
	mux.HandleFunc("/jwks", idp.handleJWKS)
	mux.HandleFunc("/token", idp.handleToken)
	idp.server = httptest.NewServer(mux)
	idp.issuer = idp.server.URL
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *mockIdP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	atomic.AddInt64(&idp.discoveryHits, 1)
	doc := map[string]any{
		"issuer":                                idp.issuer,
		"authorization_endpoint":                idp.issuer + "/authorize",
		"token_endpoint":                        idp.issuer + "/token",
		"jwks_uri":                              idp.issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
	}
	writeJSON(w, doc)
}

func (idp *mockIdP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	atomic.AddInt64(&idp.jwksHits, 1)
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       &idp.key.PublicKey,
		KeyID:     idp.keyID,
		Algorithm: "RS256",
		Use:       "sig",
	}}}
	writeJSON(w, set)
}

func (idp *mockIdP) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if idp.expectedChallenge != "" {
		if pkceChallenge(r.FormValue("code_verifier")) != idp.expectedChallenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
	}
	iss := idp.issuer
	if idp.issuerOverride != "" {
		iss = idp.issuerOverride
	}
	claims := map[string]any{
		"iss":                iss,
		"sub":                idp.subject,
		"aud":                idp.audience,
		"exp":                idp.expiry.Unix(),
		"iat":                time.Now().Add(-time.Minute).Unix(),
		"nonce":              idp.nonce,
		"email":              idp.email,
		"email_verified":     idp.emailVerified,
		"preferred_username": idp.username,
		"name":               idp.name,
	}
	for k, v := range idp.extraClaims {
		claims[k] = v
	}
	resp := map[string]any{
		"access_token": "mock-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if !idp.omitIDToken {
		resp["id_token"] = idp.signJWT(claims)
	}
	writeJSON(w, resp)
}

func (idp *mockIdP) signJWT(claims map[string]any) string {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: idp.signKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", idp.keyID),
	)
	if err != nil {
		panic(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// noProxyClient returns an HTTP client with a proxy-free transport so loopback
// requests to the mock IdP are never routed through an ambient proxy.
func noProxyClient() *http.Client {
	return &http.Client{Transport: &http.Transport{}}
}

func (idp *mockIdP) newProvider(t *testing.T, mutate func(*OIDCConfig)) *OIDCProvider {
	t.Helper()
	cfg := OIDCConfig{
		Issuer:       idp.issuer,
		ClientID:     idp.clientID,
		ClientSecret: "test-secret",
		RedirectURL:  "https://app.example.com/api/v4/auth/method/builtin.oidc/callback",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := NewOIDCProvider(cfg, WithHTTPClient(noProxyClient()))
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	return p
}

func TestOIDC_HappyPath(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "nonce-xyz"
	p := idp.newProvider(t, nil)

	id, err := p.ExchangeAndVerify(context.Background(), "code-1", "verifier-1", "nonce-xyz")
	if err != nil {
		t.Fatalf("ExchangeAndVerify: %v", err)
	}
	if id.Issuer != idp.issuer {
		t.Errorf("Issuer = %q, want %q", id.Issuer, idp.issuer)
	}
	if id.Subject != "sub-abc-123" {
		t.Errorf("Subject = %q", id.Subject)
	}
	if id.Email != "alice@example.com" || !id.EmailVerified {
		t.Errorf("Email/EmailVerified = %q/%v", id.Email, id.EmailVerified)
	}
	if id.Username != "alice" || id.Name != "Alice Example" {
		t.Errorf("Username/Name = %q/%q", id.Username, id.Name)
	}
}

func TestOIDC_BadSignature(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "n"
	// Sign with a key that is NOT in the advertised JWKS.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp.signKey = other
	p := idp.newProvider(t, nil)

	if _, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n"); err == nil {
		t.Fatal("expected verification to fail for a token signed by an unknown key")
	}
}

func TestOIDC_WrongAudience(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "n"
	idp.audience = "some-other-client"
	p := idp.newProvider(t, nil)

	if _, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n"); err == nil {
		t.Fatal("expected verification to fail when aud != client_id")
	}
}

func TestOIDC_WrongIssuer(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "n"
	idp.issuerOverride = "https://evil.example.com"
	p := idp.newProvider(t, nil)

	if _, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n"); err == nil {
		t.Fatal("expected verification to fail when iss != issuer")
	}
}

func TestOIDC_Expired(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "n"
	idp.expiry = time.Now().Add(-time.Hour)
	p := idp.newProvider(t, nil)

	if _, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n"); err == nil {
		t.Fatal("expected verification to fail for an expired id_token")
	}
}

func TestOIDC_NonceMismatch(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "attacker-replayed-nonce"
	p := idp.newProvider(t, nil)

	_, err := p.ExchangeAndVerify(context.Background(), "c", "v", "expected-nonce")
	if err == nil {
		t.Fatal("expected verification to fail when id_token nonce != expectedNonce")
	}
	if !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("error should mention nonce, got: %v", err)
	}
}

func TestOIDC_MissingIDToken(t *testing.T) {
	idp := newMockIdP(t)
	idp.omitIDToken = true
	p := idp.newProvider(t, nil)

	if _, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n"); err == nil {
		t.Fatal("expected error when token response has no id_token")
	}
}

func TestOIDC_PKCE_EndToEnd(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "n"
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	idp.expectedChallenge = challenge
	p := idp.newProvider(t, nil)

	// Correct verifier: token endpoint accepts, verification succeeds.
	if _, err := p.ExchangeAndVerify(context.Background(), "code", verifier, "n"); err != nil {
		t.Fatalf("exchange with matching PKCE verifier should succeed: %v", err)
	}
	// Wrong verifier: token endpoint rejects the code (400), exchange fails.
	if _, err := p.ExchangeAndVerify(context.Background(), "code", "wrong-verifier", "n"); err == nil {
		t.Fatal("exchange with a non-matching PKCE verifier must fail")
	}
}

func TestOIDC_BuildAuthURL(t *testing.T) {
	idp := newMockIdP(t)
	p := idp.newProvider(t, nil)

	raw, err := p.BuildAuthURL("state-123", "nonce-456", "challenge-789")
	if err != nil {
		t.Fatalf("BuildAuthURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if got, want := u.Path, "/authorize"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	q := u.Query()
	checks := map[string]string{
		"response_type":         "code",
		"client_id":             idp.clientID,
		"redirect_uri":          "https://app.example.com/api/v4/auth/method/builtin.oidc/callback",
		"state":                 "state-123",
		"nonce":                 "nonce-456",
		"code_challenge":        "challenge-789",
		"code_challenge_method": "S256",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("query %q = %q, want %q", k, got, want)
		}
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Errorf("scope %q must contain openid", q.Get("scope"))
	}
}

func TestOIDC_DiscoveryCachedAcrossCalls(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "n"
	p := idp.newProvider(t, nil)

	for i := 0; i < 3; i++ {
		if _, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if hits := atomic.LoadInt64(&idp.discoveryHits); hits != 1 {
		t.Fatalf("discovery should be fetched exactly once (cached), got %d hits", hits)
	}
}

func TestOIDC_CustomClaimNames(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "n"
	idp.username = "" // standard preferred_username left blank
	idp.name = ""
	idp.extraClaims = map[string]any{
		"nickname":  "al",
		"full_name": "Alice From Nickname",
	}
	p := idp.newProvider(t, func(cfg *OIDCConfig) {
		cfg.Claims = ClaimNames{Username: "nickname", Name: "full_name"}
	})

	id, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n")
	if err != nil {
		t.Fatalf("ExchangeAndVerify: %v", err)
	}
	if id.Username != "al" {
		t.Errorf("Username via custom claim = %q, want %q", id.Username, "al")
	}
	if id.Name != "Alice From Nickname" {
		t.Errorf("Name via custom claim = %q", id.Name)
	}
}

func TestOIDC_EmailVerifiedAsString(t *testing.T) {
	idp := newMockIdP(t)
	idp.nonce = "n"
	idp.extraClaims = map[string]any{"email_verified": "true"} // string form
	p := idp.newProvider(t, nil)

	id, err := p.ExchangeAndVerify(context.Background(), "c", "v", "n")
	if err != nil {
		t.Fatalf("ExchangeAndVerify: %v", err)
	}
	if !id.EmailVerified {
		t.Error("email_verified encoded as the string \"true\" should map to true")
	}
}
