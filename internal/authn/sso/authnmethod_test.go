package sso

import (
	"context"
	"net/url"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
)

func TestStartFlowParksStateAndBuildsAuthURL(t *testing.T) {
	SetHTTPClient(noProxyClient())
	t.Cleanup(func() { SetHTTPClient(nil) })
	idp := newMockIdP(t)
	pc := sampleConfig(idp)

	before := DefaultStateStore.Len()
	authURL, _, err := StartFlow(pc, "https://app/api/v4/auth/method/sso:casdoor/callback", true, 7, "browser-client-1")
	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	if DefaultStateStore.Len() != before+1 {
		t.Fatal("StartFlow must park exactly one state")
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()
	state := q.Get("state")
	if state == "" || q.Get("code_challenge") == "" || q.Get("nonce") == "" {
		t.Fatalf("auth url missing state/nonce/pkce: %v", q)
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatal("PKCE must be S256")
	}
	// The parked state carries the linking context and the secrets the callback
	// needs, and is single-use.
	st, ok := DefaultStateStore.Take(state)
	if !ok {
		t.Fatal("parked state must be retrievable by the state id from the auth url")
	}
	if st.ProviderKey != pc.Key {
		t.Errorf("ProviderKey = %q, want %q", st.ProviderKey, pc.Key)
	}
	if st.LinkUserID != 7 {
		t.Errorf("LinkUserID = %d, want 7", st.LinkUserID)
	}
	if st.ClientID != "browser-client-1" {
		t.Errorf("ClientID = %q, want %q", st.ClientID, "browser-client-1")
	}
	if !st.Remember {
		t.Error("Remember flag lost")
	}
	if st.Nonce != q.Get("nonce") {
		t.Error("stored nonce must equal the one sent to the IdP")
	}
	if st.PKCEVerifier == "" {
		t.Error("PKCE verifier must be stored server-side")
	}
	if _, again := DefaultStateStore.Take(state); again {
		t.Fatal("state must be single-use")
	}
}

func TestAdapterBeginParksLoginModeState(t *testing.T) {
	SetHTTPClient(noProxyClient())
	t.Cleanup(func() { SetHTTPClient(nil) })
	idp := newMockIdP(t)
	m := &providerMethod{pc: sampleConfig(idp)}

	if m.MethodInfo().Kind != ext.AuthFederated {
		t.Fatal("an SSO method must classify as federated")
	}

	ch, err := m.Begin(context.Background(), "", ext.AuthNRequest{
		RedirectURI: "https://app/api/v4/auth/method/sso:casdoor/callback",
		Params:      map[string]string{"remember": "true"},
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if ch.RedirectURL == "" {
		t.Fatal("federated Begin must return an authorization RedirectURL")
	}
	u, _ := url.Parse(ch.RedirectURL)
	st, ok := DefaultStateStore.Take(u.Query().Get("state"))
	if !ok {
		t.Fatal("Begin must park the flow state")
	}
	if st.LinkUserID != 0 {
		t.Fatal("a plain login Begin must park LinkUserID=0 (login, not link, mode)")
	}
	if !st.Remember {
		t.Fatal("remember=true must be carried through the redirect")
	}
}

func TestAdapterFinishAlwaysErrors(t *testing.T) {
	m := &providerMethod{pc: ProviderConfig{Key: "k", Name: "K"}}
	res, err := m.Finish(context.Background(), ext.AuthNResponse{})
	if err == nil {
		t.Fatal("SSO Finish must error: completion goes through the browser callback, never the generic finish")
	}
	if res.Verified {
		t.Fatal("a Finish that errors must not report a verified identity")
	}
}

func TestRegisterProviderMethodIsResolvableAndTrusted(t *testing.T) {
	pc := ProviderConfig{Key: "reg", Name: "Reg", Issuer: "https://i", ClientID: "c", Enabled: true}
	RegisterProviderMethod(pc)
	t.Cleanup(func() { DeregisterProviderMethod(pc.Key) })

	got, release, err := extension.DefaultRegistry.GetAuthNMethod(MethodID(pc.Key))
	if err != nil {
		t.Fatalf("registered method must resolve: %v", err)
	}
	defer release()
	if got.MethodInfo().Kind != ext.AuthFederated {
		t.Fatal("resolved method must be federated")
	}
	if !extension.DefaultRegistry.AuthNMethodTrusted(MethodID(pc.Key)) {
		t.Fatal("SSO provider methods are registered on the trusted built-in path")
	}

	DeregisterProviderMethod(pc.Key)
	if _, _, err := extension.DefaultRegistry.GetAuthNMethod(MethodID(pc.Key)); err == nil {
		t.Fatal("deregistered method must no longer resolve")
	}
}
