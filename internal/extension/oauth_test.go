package extension

import (
	"context"
	"net/url"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// stubAuthProvider is an in-process ext.AuthProvider test double (WS-8): v4
// ships no real network drive/OAuth plugin yet, so the wizard mechanism is
// proven against this stub instead. Exchange/Refresh don't hit a real vendor —
// that round trip is covered separately, over a real gRPC subprocess, by
// internal/extension/grpc's TestAuthProviderGRPCRoundTrip. This test's job is
// the HOST side: state minting/validation and the wizard session's advance.
type stubAuthProvider struct {
	id string
}

func (p *stubAuthProvider) Manifest() ext.Manifest {
	return ext.Manifest{ID: p.id, Kinds: []ext.Kind{ext.KindAuthProvider}}
}
func (p *stubAuthProvider) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *stubAuthProvider) Shutdown(ctx context.Context) error         { return nil }

func (p *stubAuthProvider) AuthURL(ctx context.Context, req ext.AuthURLRequest) (string, error) {
	q := url.Values{"state": {req.State}, "redirect_uri": {req.RedirectURI}}
	return "https://vendor.example.com/authorize?" + q.Encode(), nil
}

func (p *stubAuthProvider) Exchange(ctx context.Context, req ext.ExchangeRequest) (ext.Token, error) {
	return ext.Token{AccessToken: "at-" + req.Code, RefreshToken: "rt-1", ExpiresIn: 3600,
		Raw: map[string]any{"account_id": "acct-1"}}, nil
}

func (p *stubAuthProvider) Refresh(ctx context.Context, refreshToken string) (ext.Token, error) {
	return ext.Token{AccessToken: "at-refreshed", RefreshToken: refreshToken}, nil
}

func oauthTestSteps(providerID string) []ext.WizardStep {
	return []ext.WizardStep{
		{ID: "oauth", Type: ext.WizardOAuth, Provider: providerID, Scopes: []string{"read"}},
		{ID: "done", Type: ext.WizardDone},
	}
}

const testRedirectURI = "https://alist.example.com/api/v4/extensions/oauth/callback"

// TestWizardOAuthHappyPath drives the full host mechanism: start the wizard,
// land on the oauth step and get an AuthURL carrying a state, simulate the
// vendor's callback with that state, and confirm Next then advances past the
// step with the exchanged token visible in the merged answers (i.e. it
// "lands in the wizard context" for a caller to use when creating the
// resulting storage's credentials).
func TestWizardOAuthHappyPath(t *testing.T) {
	const providerID = "com.example.oauthhappy"
	RegisterAuthProviderInstance(providerID, &stubAuthProvider{id: providerID})
	t.Cleanup(func() { RegisterAuthProviderInstance(providerID, nil) })

	e := NewWizardEngine(time.Minute)
	view, err := e.Start(context.Background(), "ext.oauth-happy", oauthTestSteps(providerID), testRedirectURI)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if view.Step == nil || view.Step.ID != "oauth" {
		t.Fatalf("expected to land on oauth step: %+v", view)
	}
	if view.AuthURL == "" {
		t.Fatalf("oauth step must carry an AuthURL: %+v", view)
	}
	u, err := url.Parse(view.AuthURL)
	if err != nil {
		t.Fatalf("AuthURL unparseable: %v", err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("AuthURL must carry a state param: %q", view.AuthURL)
	}
	if got := u.Query().Get("redirect_uri"); got != testRedirectURI {
		t.Fatalf("AuthURL redirect_uri = %q, want %q", got, testRedirectURI)
	}

	// Polling Next before the callback fires must not advance, and must reuse
	// the SAME AuthURL/state rather than minting a new one on every poll.
	polled, err := e.Next(context.Background(), view.SessionID, nil, testRedirectURI)
	if err != nil {
		t.Fatalf("Next (poll): %v", err)
	}
	if polled.Done || polled.Step == nil || polled.Step.ID != "oauth" {
		t.Fatalf("poll before callback must not advance: %+v", polled)
	}
	if polled.AuthURL != view.AuthURL {
		t.Fatalf("poll must reuse the cached AuthURL, got new one: %q vs %q", polled.AuthURL, view.AuthURL)
	}

	// Simulate the vendor redirecting the browser to the callback with the
	// state AuthURL minted.
	cbView, err := e.HandleOAuthCallback(context.Background(), "auth-code-xyz", state)
	if err != nil {
		t.Fatalf("HandleOAuthCallback: %v", err)
	}
	if cbView.Done {
		t.Fatalf("callback must not itself advance the session: %+v", cbView)
	}
	if cbView.Answers == nil || cbView.Answers["token"] == nil {
		t.Fatalf("callback response must carry the exchanged token: %+v", cbView)
	}
	tokMap, ok := cbView.Answers["token"].(map[string]any)
	if !ok || tokMap["access_token"] != "at-auth-code-xyz" {
		t.Fatalf("callback token = %+v", cbView.Answers["token"])
	}

	// Now the main tab's Next() call advances past the completed oauth step.
	final, err := e.Next(context.Background(), view.SessionID, nil, testRedirectURI)
	if err != nil {
		t.Fatalf("Next (after callback): %v", err)
	}
	if !final.Done {
		t.Fatalf("expected the wizard to reach done after the oauth step completes: %+v", final)
	}
	tok, ok := final.Answers["token"].(map[string]any)
	if !ok {
		t.Fatalf("done response must carry the token in merged answers: %+v", final.Answers)
	}
	if tok["access_token"] != "at-auth-code-xyz" || tok["refresh_token"] != "rt-1" {
		t.Fatalf("merged token = %+v", tok)
	}
	raw, ok := tok["raw"].(map[string]any)
	if !ok || raw["account_id"] != "acct-1" {
		t.Fatalf("merged token must round-trip the provider's opaque Raw payload: %+v", tok)
	}
}

// TestWizardOAuthCallbackRejectsUnknownState pins the anti-CSRF guard: a
// callback carrying a state the engine never minted (forged, or from a
// different/unrelated flow) must be rejected outright, and must not touch any
// session.
func TestWizardOAuthCallbackRejectsUnknownState(t *testing.T) {
	const providerID = "com.example.oauthbadstate"
	RegisterAuthProviderInstance(providerID, &stubAuthProvider{id: providerID})
	t.Cleanup(func() { RegisterAuthProviderInstance(providerID, nil) })

	e := NewWizardEngine(time.Minute)
	view, err := e.Start(context.Background(), "ext.oauth-bad", oauthTestSteps(providerID), testRedirectURI)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if view.AuthURL == "" {
		t.Fatalf("expected an AuthURL: %+v", view)
	}

	if _, err := e.HandleOAuthCallback(context.Background(), "some-code", "forged-state-that-was-never-minted"); err != ErrOAuthStateInvalid {
		t.Fatalf("unknown state must be rejected with ErrOAuthStateInvalid, got %v", err)
	}

	// The wizard must be unaffected: still sitting on the un-advanced,
	// un-completed oauth step.
	polled, err := e.Next(context.Background(), view.SessionID, nil, testRedirectURI)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if polled.Done || polled.Step == nil || polled.Step.ID != "oauth" {
		t.Fatalf("session must be untouched by a rejected callback: %+v", polled)
	}
}

// TestWizardOAuthCallbackRejectsMissingState covers the degenerate/empty
// input a naive or malicious caller might send.
func TestWizardOAuthCallbackRejectsMissingState(t *testing.T) {
	e := NewWizardEngine(time.Minute)
	if _, err := e.HandleOAuthCallback(context.Background(), "code", ""); err != ErrOAuthStateInvalid {
		t.Fatalf("empty state must be rejected with ErrOAuthStateInvalid, got %v", err)
	}
}

// TestWizardOAuthCallbackStateIsSingleUse pins the anti-replay guard: the same
// state must not be redeemable twice.
func TestWizardOAuthCallbackStateIsSingleUse(t *testing.T) {
	const providerID = "com.example.oauthreplay"
	RegisterAuthProviderInstance(providerID, &stubAuthProvider{id: providerID})
	t.Cleanup(func() { RegisterAuthProviderInstance(providerID, nil) })

	e := NewWizardEngine(time.Minute)
	view, err := e.Start(context.Background(), "ext.oauth-replay", oauthTestSteps(providerID), testRedirectURI)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	u, _ := url.Parse(view.AuthURL)
	state := u.Query().Get("state")

	if _, err := e.HandleOAuthCallback(context.Background(), "code-1", state); err != nil {
		t.Fatalf("first callback: %v", err)
	}
	if _, err := e.HandleOAuthCallback(context.Background(), "code-2", state); err != ErrOAuthStateInvalid {
		t.Fatalf("replaying the same state must be rejected, got %v", err)
	}
}

// TestWizardOAuthCallbackRejectsExpiredState pins the TTL bound: a state whose
// wizard session (and thus the state itself, bound to the same TTL) has
// expired must be rejected rather than silently exchanged.
func TestWizardOAuthCallbackRejectsExpiredState(t *testing.T) {
	const providerID = "com.example.oauthexpiry"
	RegisterAuthProviderInstance(providerID, &stubAuthProvider{id: providerID})
	t.Cleanup(func() { RegisterAuthProviderInstance(providerID, nil) })

	e := NewWizardEngine(10 * time.Millisecond)
	view, err := e.Start(context.Background(), "ext.oauth-expiry", oauthTestSteps(providerID), testRedirectURI)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	u, _ := url.Parse(view.AuthURL)
	state := u.Query().Get("state")

	time.Sleep(30 * time.Millisecond)

	if _, err := e.HandleOAuthCallback(context.Background(), "code", state); err != ErrOAuthStateInvalid {
		t.Fatalf("expired state must be rejected, got %v", err)
	}
}

// TestGetAuthProviderUnknownID confirms the registry seam rejects an unknown
// or non-AuthProvider id cleanly.
func TestGetAuthProviderUnknownID(t *testing.T) {
	r := NewRegistry()
	if _, _, err := r.GetAuthProvider("nope"); err == nil {
		t.Fatal("expected an error for an unregistered auth provider id")
	}
}

// TestGetAuthProviderInstance confirms RegisterAuthProviderInstance is the
// seam GetAuthProvider resolves through for an in-process double.
func TestGetAuthProviderInstance(t *testing.T) {
	const id = "com.example.directinstance"
	RegisterAuthProviderInstance(id, &stubAuthProvider{id: id})
	t.Cleanup(func() { RegisterAuthProviderInstance(id, nil) })

	r := NewRegistry()
	p, release, err := r.GetAuthProvider(id)
	if err != nil {
		t.Fatalf("GetAuthProvider: %v", err)
	}
	defer release()
	if p.Manifest().ID != id {
		t.Fatalf("got provider for wrong id: %+v", p.Manifest())
	}
}
