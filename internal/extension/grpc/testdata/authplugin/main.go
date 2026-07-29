// Command authplugin serves a synthetic Kind=AuthProvider extension as a
// go-plugin gRPC plugin (WS-8's mechanism-proving test double — v4 ships no
// real network drive/OAuth plugin yet). It exercises the full AuthProvider
// round trip: AuthURL builds a fake vendor authorization URL; Exchange and
// Refresh call a vendor TOKEN endpoint directly from inside this process
// (never through any broker), using client_id/client_secret it was configured
// with. The vendor endpoint itself is a httptest.Server the test spins up in
// the parent process; its base URL is passed down via the
// AUTHPLUGIN_TOKEN_ENDPOINT environment variable (inherited from the parent,
// since exec.Cmd.Env is left nil by host.LaunchAuthProvider).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

const (
	envTokenEndpoint = "AUTHPLUGIN_TOKEN_ENDPOINT"
	clientID         = "test-client-id"
	clientSecret     = "test-client-secret"
)

type authPlugin struct {
	tokenEndpoint string
}

var _ ext.AuthProvider = (*authPlugin)(nil)

func (p *authPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testauth",
		Name:    map[string]string{"en": "Test Auth Provider"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindAuthProvider},
	}
}

func (p *authPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *authPlugin) Shutdown(ctx context.Context) error         { return nil }

// AuthURL builds the (fake) vendor authorization-page URL. No network call —
// vendors don't require one to build this URL, only to redeem a code.
func (p *authPlugin) AuthURL(ctx context.Context, req ext.AuthURLRequest) (string, error) {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", req.RedirectURI)
	q.Set("state", req.State)
	if len(req.Scopes) > 0 {
		q.Set("scope", strings.Join(req.Scopes, " "))
	}
	return p.tokenEndpoint + "/authorize?" + q.Encode(), nil
}

// Exchange trades an authorization code for a token by POSTing directly to
// the vendor's (mock) token endpoint — locally, inside this plugin process,
// using this plugin's own client_id/client_secret. No broker involved.
func (p *authPlugin) Exchange(ctx context.Context, req ext.ExchangeRequest) (ext.Token, error) {
	return p.callToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {req.Code},
		"redirect_uri":  {req.RedirectURI},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
}

func (p *authPlugin) Refresh(ctx context.Context, refreshToken string) (ext.Token, error) {
	return p.callToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
}

func (p *authPlugin) callToken(ctx context.Context, form url.Values) (ext.Token, error) {
	if p.tokenEndpoint == "" {
		return ext.Token{}, fmt.Errorf("authplugin: %s not set", envTokenEndpoint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenEndpoint+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return ext.Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ext.Token{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ext.Token{}, fmt.Errorf("authplugin: token endpoint status %d", resp.StatusCode)
	}
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		AccountID    string `json:"account_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ext.Token{}, err
	}
	return ext.Token{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		ExpiresIn:    body.ExpiresIn,
		TokenType:    body.TokenType,
		Raw:          map[string]any{"account_id": body.AccountID},
	}, nil
}

func main() {
	tokenEndpoint := os.Getenv(envTokenEndpoint)
	if tokenEndpoint == "" {
		// Keep the process alive long enough to report a clear manifest/error
		// instead of crashing the go-plugin handshake outright.
		fmt.Fprintln(os.Stderr, "authplugin: "+envTokenEndpoint+" not set")
	}
	grpcplugin.ServeAuthProvider(&authPlugin{tokenEndpoint: tokenEndpoint})
}
