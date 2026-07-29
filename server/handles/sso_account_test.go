package handles

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/alist-org/alist/v3/internal/authn/sso"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
)

func TestSSOLinkBeginParksLinkState(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })
	idp := newSSOMockIdP(t)
	setSSOProvider(t, sso.ProviderConfig{Key: "lprov", Name: "L", Issuer: idp.issuer, ClientID: idp.clientID, Enabled: true})

	user := mustAuthNUser(t, "sso-link-begin", model.GENERAL, false)
	w, c := authedCtx(t, user, nil, gin.Params{{Key: "id", Value: "sso:lprov"}})
	SSOLinkBegin(c)

	e := decodeEnvelope(t, w)
	if e.Code != 200 {
		t.Fatalf("link begin failed: %s", w.Body.String())
	}
	var data struct {
		RedirectURL string `json:"redirect_url"`
	}
	_ = json.Unmarshal(e.Data, &data)
	if data.RedirectURL == "" {
		t.Fatal("link begin must return an authorization redirect_url")
	}
	u, _ := url.Parse(data.RedirectURL)
	st, ok := sso.DefaultStateStore.Take(u.Query().Get("state"))
	if !ok {
		t.Fatal("link begin must park a flow state")
	}
	if st.LinkUserID != user.ID {
		t.Fatalf("link state must bind LinkUserID to the caller, got %d want %d", st.LinkUserID, user.ID)
	}
}

func TestSSOLinkBeginGuestRefused(t *testing.T) {
	guest := &model.User{Role: model.Roles{model.GUEST}}
	w, c := authedCtx(t, guest, nil, gin.Params{{Key: "id", Value: "sso:lprov"}})
	SSOLinkBegin(c)
	if e := decodeEnvelope(t, w); e.Code == 200 {
		t.Fatal("the guest user must not start an SSO link flow")
	}
}

func TestSSOListLinks(t *testing.T) {
	setSSOProvider(t, sso.ProviderConfig{Key: "prov", Name: "Provider Display", Issuer: "https://i", ClientID: "c", Enabled: true})
	user := mustAuthNUser(t, "sso-list", model.GENERAL, false)
	setUserSSOID(t, user, "prov:subject-9")

	w, c := authedCtx(t, user, nil, nil)
	SSOListLinks(c)
	e := decodeEnvelope(t, w)
	var links []ssoLinkView
	_ = json.Unmarshal(e.Data, &links)
	if len(links) != 1 {
		t.Fatalf("expected one linked provider, got %+v", links)
	}
	if links[0].ProviderKey != "prov" || links[0].Subject != "subject-9" {
		t.Fatalf("wrong link view: %+v", links[0])
	}
	if links[0].ProviderName != "Provider Display" {
		t.Fatalf("link view should carry the provider display name, got %q", links[0].ProviderName)
	}
}

func TestSSOUnlink(t *testing.T) {
	user := mustAuthNUser(t, "sso-unlink", model.GENERAL, false)
	setUserSSOID(t, user, "prov:sub-x")

	// A key that does not match the linked provider is a 404 and changes nothing.
	w, c := authedCtx(t, user, nil, gin.Params{{Key: "key", Value: "other"}})
	SSOUnlink(c)
	if e := decodeEnvelope(t, w); e.Code == 200 {
		t.Fatal("unlinking a non-matching provider must 404")
	}
	if reloadUser(t, user.ID).SsoID != "prov:sub-x" {
		t.Fatal("a failed unlink must not clear the link")
	}

	// The matching key clears the link.
	w, c = authedCtx(t, user, nil, gin.Params{{Key: "key", Value: "prov"}})
	SSOUnlink(c)
	if e := decodeEnvelope(t, w); e.Code != 200 {
		t.Fatalf("unlink failed: %s", w.Body.String())
	}
	if reloadUser(t, user.ID).SsoID != "" {
		t.Fatal("unlink must clear the caller's SsoID")
	}
}
