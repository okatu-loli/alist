package ext

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeAuthProvider is a minimal in-process ext.AuthProvider used to pin the
// interface's compile-time shape (WS-8).
type fakeAuthProvider struct{}

func (fakeAuthProvider) Manifest() Manifest                     { return Manifest{ID: "com.example.auth"} }
func (fakeAuthProvider) Init(ctx context.Context, h Host) error { return nil }
func (fakeAuthProvider) Shutdown(ctx context.Context) error     { return nil }

func (fakeAuthProvider) AuthURL(ctx context.Context, req AuthURLRequest) (string, error) {
	return "https://vendor.example.com/authorize?state=" + req.State, nil
}

func (fakeAuthProvider) Exchange(ctx context.Context, req ExchangeRequest) (Token, error) {
	return Token{AccessToken: "at-" + req.Code, RefreshToken: "rt-1", ExpiresIn: 3600}, nil
}

func (fakeAuthProvider) Refresh(ctx context.Context, refreshToken string) (Token, error) {
	return Token{AccessToken: "at-refreshed", RefreshToken: refreshToken, ExpiresIn: 3600}, nil
}

var _ AuthProvider = fakeAuthProvider{}

func TestAuthProviderInterfaceShape(t *testing.T) {
	var p AuthProvider = fakeAuthProvider{}
	url, err := p.AuthURL(context.Background(), AuthURLRequest{State: "s1", RedirectURI: "https://host/callback"})
	if err != nil {
		t.Fatalf("AuthURL: %v", err)
	}
	if url == "" {
		t.Fatal("AuthURL returned empty string")
	}
	tok, err := p.Exchange(context.Background(), ExchangeRequest{Code: "c1", RedirectURI: "https://host/callback"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "at-c1" {
		t.Fatalf("Exchange token = %+v", tok)
	}
	tok2, err := p.Refresh(context.Background(), tok.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok2.RefreshToken != tok.RefreshToken {
		t.Fatalf("Refresh should round-trip the refresh token: %+v", tok2)
	}
}

// TestTokenRoundTrip pins Token's JSON shape, including Raw (the
// provider-specific passthrough the host round-trips into a storage's
// Addition config verbatim).
func TestTokenRoundTrip(t *testing.T) {
	tok := Token{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresIn:    3600,
		TokenType:    "Bearer",
		Raw:          map[string]any{"account_id": "u1"},
	}
	b, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Token
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AccessToken != tok.AccessToken || got.Raw["account_id"] != "u1" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
