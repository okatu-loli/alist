package grpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// newVendorTokenServer starts a fake OAuth vendor token endpoint. It accepts
// authorization_code and refresh_token grants and returns a JSON token body,
// mirroring a real vendor's /token endpoint closely enough to exercise
// authplugin's Exchange/Refresh over the wire.
func newVendorTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.FormValue("client_id") != "test-client-id" || r.FormValue("client_secret") != "test-client-secret" {
			http.Error(w, "bad client credentials", http.StatusUnauthorized)
			return
		}
		var accessToken string
		switch r.FormValue("grant_type") {
		case "authorization_code":
			code := r.FormValue("code")
			if code == "" {
				http.Error(w, "missing code", http.StatusBadRequest)
				return
			}
			accessToken = "at-for-" + code
		case "refresh_token":
			rt := r.FormValue("refresh_token")
			if rt == "" {
				http.Error(w, "missing refresh_token", http.StatusBadRequest)
				return
			}
			accessToken = "at-refreshed-from-" + rt
		default:
			http.Error(w, "unsupported grant_type", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  accessToken,
			"refresh_token": "rt-1",
			"expires_in":    3600,
			"token_type":    "Bearer",
			"account_id":    "acct-42",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// launchAuthPlugin starts the authplugin fixture with AUTHPLUGIN_TOKEN_ENDPOINT
// pointed at vendorURL. host.LaunchAuthProvider uses exec.Cmd with a nil Env,
// so the child inherits the parent's environment — t.Setenv here is visible to
// the subprocess it launches.
func launchAuthPlugin(t *testing.T, vendorURL string) *host.AuthClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	t.Setenv("AUTHPLUGIN_TOKEN_ENDPOINT", vendorURL)
	c, err := host.LaunchAuthProvider(context.Background(), exec.Command(authPluginBin), nil)
	if err != nil {
		t.Fatalf("launch auth plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestAuthProviderGRPCRoundTrip is the WS-8 gRPC round-trip gate: drive
// AuthURL/Exchange/Refresh against a real go-plugin subprocess and confirm the
// token exchange happens locally (the subprocess calls the vendor token
// endpoint directly, never through any broker in this test's process tree) and
// round-trips correctly across the wire, including the opaque Raw payload.
func TestAuthProviderGRPCRoundTrip(t *testing.T) {
	vendor := newVendorTokenServer(t)
	c := launchAuthPlugin(t, vendor.URL)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testauth" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	authURL, err := c.AuthURL(ctx, ext.AuthURLRequest{
		State:       "state-123",
		RedirectURI: "https://alist.example.com/api/v4/extensions/oauth/callback",
		Scopes:      []string{"read", "write"},
	})
	if err != nil {
		t.Fatalf("AuthURL: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("AuthURL returned unparseable URL %q: %v", authURL, err)
	}
	if got := u.Query().Get("state"); got != "state-123" {
		t.Fatalf("AuthURL must round-trip state, got %q in %q", got, authURL)
	}
	if !strings.HasPrefix(authURL, vendor.URL) {
		t.Fatalf("AuthURL = %q, want vendor-hosted (%q)", authURL, vendor.URL)
	}

	tok, err := c.Exchange(ctx, ext.ExchangeRequest{
		Code:        "auth-code-1",
		RedirectURI: "https://alist.example.com/api/v4/extensions/oauth/callback",
	})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "at-for-auth-code-1" {
		t.Fatalf("Exchange access_token = %q", tok.AccessToken)
	}
	if tok.RefreshToken != "rt-1" || tok.ExpiresIn != 3600 || tok.TokenType != "Bearer" {
		t.Fatalf("Exchange token = %+v", tok)
	}
	if tok.Raw["account_id"] != "acct-42" {
		t.Fatalf("Exchange must round-trip the opaque Raw payload, got %+v", tok.Raw)
	}

	refreshed, err := c.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.AccessToken != "at-refreshed-from-rt-1" {
		t.Fatalf("Refresh access_token = %q", refreshed.AccessToken)
	}
}

// TestAuthProviderGRPCExchangeErrorPropagates confirms a vendor-side failure
// (bad client credentials here, standing in for "bad/expired code") surfaces
// through the gRPC boundary as a plain error, not a silently empty token.
func TestAuthProviderGRPCExchangeErrorPropagates(t *testing.T) {
	// A vendor server that always 401s regardless of client credentials.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := launchAuthPlugin(t, srv.URL)
	_, err := c.Exchange(context.Background(), ext.ExchangeRequest{Code: "bad-code", RedirectURI: "https://alist.example.com/callback"})
	if err == nil {
		t.Fatal("Exchange against a rejecting vendor must return an error")
	}
}

// TestAuthProviderMissingTokenEndpoint confirms a misconfigured plugin (no
// vendor endpoint reachable) fails Exchange cleanly rather than hanging or
// panicking the subprocess.
func TestAuthProviderMissingTokenEndpoint(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	// Deliberately do not set AUTHPLUGIN_TOKEN_ENDPOINT.
	if v, ok := os.LookupEnv("AUTHPLUGIN_TOKEN_ENDPOINT"); ok {
		t.Setenv("AUTHPLUGIN_TOKEN_ENDPOINT", "")
		defer t.Setenv("AUTHPLUGIN_TOKEN_ENDPOINT", v)
	}
	c, err := host.LaunchAuthProvider(context.Background(), exec.Command(authPluginBin), nil)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	_, err = c.Exchange(context.Background(), ext.ExchangeRequest{Code: "c", RedirectURI: "https://host/callback"})
	if err == nil {
		t.Fatal("Exchange with no token endpoint configured must return an error")
	}
}
