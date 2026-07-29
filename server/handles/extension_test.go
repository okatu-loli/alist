package handles

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/gin-gonic/gin"
)

// registerEnabledExtension inserts an enabled non-driver entry into the global
// registry for a handler test, and cleans it up. enabled is unexported, so the
// entry is registered then flipped via the exported Enable.
func registerEnabledExtension(t *testing.T, m ext.Manifest) {
	t.Helper()
	e := &extension.Entry{Manifest: m, Kind: ext.KindAction, Mode: extension.InstanceModeSingleton}
	if err := extension.DefaultRegistry.Register(e); err != nil {
		t.Fatal(err)
	}
	if err := extension.DefaultRegistry.Enable(m.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(m.ID) })
}

func TestExtensionContributionsHandlerReactsToDisable(t *testing.T) {
	registerEnabledExtension(t, ext.Manifest{
		ID:    "h.ext.a",
		Name:  map[string]string{"en": "A"},
		Kinds: []ext.Kind{ext.KindAction},
		Contributions: ext.Contributions{
			MenuActions: []ext.MenuAction{{ID: "a1", Label: map[string]string{"en": "A1"}, Target: "file"}},
		},
	})

	get := func() extension.ContributionsDoc {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/api/v4/extensions/contributions", nil)
		ExtensionContributions(c)
		var resp struct {
			Code int                        `json:"code"`
			Data extension.ContributionsDoc `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		return resp.Data
	}

	doc := get()
	found := false
	for _, e := range doc.Extensions {
		if e.ID == "h.ext.a" && len(e.Contributions.MenuActions) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("enabled extension missing from contributions: %+v", doc.Extensions)
	}

	if err := extension.DefaultRegistry.Disable("h.ext.a"); err != nil {
		t.Fatal(err)
	}
	for _, e := range get().Extensions {
		if e.ID == "h.ext.a" {
			t.Fatal("disabled extension must not appear in contributions")
		}
	}
}

type handlerInvoker struct{ called string }

func (h *handlerInvoker) InvokeAction(ctx context.Context, actionID string, args map[string]any) (any, error) {
	h.called = actionID
	return map[string]any{"ran": actionID}, nil
}

// actionChain wires the real RequireAdmin gate in front of the handler with the
// given principal, mirroring the router, so the admin gate is exercised.
func actionChain(user *model.User, id, aid string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(ctxkey.User, user) })
	r.POST("/api/v4/extensions/:id/actions/:aid", middlewares.RequireAdmin, InvokeExtensionAction)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v4/extensions/"+id+"/actions/"+aid, nil)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestInvokeActionHandlerAdminDeclared(t *testing.T) {
	registerEnabledExtension(t, ext.Manifest{
		ID:    "h.ext.act",
		Kinds: []ext.Kind{ext.KindAction},
		Contributions: ext.Contributions{
			MenuActions: []ext.MenuAction{{ID: "run", Label: map[string]string{"en": "Run"}, Target: "global"}},
		},
	})
	inv := &handlerInvoker{}
	extension.RegisterActionInvoker("h.ext.act", inv)
	t.Cleanup(func() { extension.RegisterActionInvoker("h.ext.act", nil) })

	admin := &model.User{ID: 1, Username: "admin", Role: model.Roles{model.ADMIN}}

	// declared action invokes
	if w := actionChain(admin, "h.ext.act", "run"); respCode(t, w) != 200 || inv.called != "run" {
		t.Fatalf("declared action: code=%d called=%q body=%s", respCode(t, w), inv.called, w.Body.String())
	}
	// undeclared action → 404
	if w := actionChain(admin, "h.ext.act", "nope"); respCode(t, w) != 404 {
		t.Fatalf("undeclared action must be 404, got %d: %s", respCode(t, w), w.Body.String())
	}
}

type handlerAssistProvider struct{ called string }

func (h *handlerAssistProvider) Assist(ctx context.Context, formID string, input map[string]any) (any, error) {
	h.called = formID
	return map[string]any{"ran": formID}, nil
}

// assistChain wires the real RequireAdmin gate in front of the handler with
// the given principal, mirroring the router (server/router_v4.go's
// /:id/assist/:fid route). Mirrors actionChain one route over.
func assistChain(user *model.User, id, fid string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(ctxkey.User, user) })
	r.POST("/api/v4/extensions/:id/assist/:fid", middlewares.RequireAdmin, SubmitExtensionAssist)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v4/extensions/"+id+"/assist/"+fid, nil)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// TestSubmitAssistHandlerAdminDeclared proves the SubmitExtensionAssist
// handler (server/handles/extension.go, wired but previously untested — Task
// 3) routes a declared assist form to its in-process AssistProvider and
// rejects an undeclared one with 404, mirroring
// TestInvokeActionHandlerAdminDeclared one route over.
func TestSubmitAssistHandlerAdminDeclared(t *testing.T) {
	registerEnabledExtension(t, ext.Manifest{
		ID:    "h.ext.assist",
		Kinds: []ext.Kind{ext.KindAction},
		Contributions: ext.Contributions{
			AssistForms: []ext.AssistForm{{ID: "probe", Title: map[string]string{"en": "Probe"}}},
		},
	})
	p := &handlerAssistProvider{}
	extension.RegisterAssistProvider("h.ext.assist", p)
	t.Cleanup(func() { extension.RegisterAssistProvider("h.ext.assist", nil) })

	admin := &model.User{ID: 1, Username: "admin", Role: model.Roles{model.ADMIN}}

	// declared form invokes
	if w := assistChain(admin, "h.ext.assist", "probe"); respCode(t, w) != 200 || p.called != "probe" {
		t.Fatalf("declared assist form: code=%d called=%q body=%s", respCode(t, w), p.called, w.Body.String())
	}
}

// TestSubmitAssistHandlerUndeclared404 proves an assist form id the
// extension never declared in Contributions.AssistForms is indistinguishable
// from a missing one to the client — 404, not 501 or 500.
func TestSubmitAssistHandlerUndeclared404(t *testing.T) {
	registerEnabledExtension(t, ext.Manifest{
		ID:    "h.ext.assist2",
		Kinds: []ext.Kind{ext.KindAction},
		Contributions: ext.Contributions{
			AssistForms: []ext.AssistForm{{ID: "probe", Title: map[string]string{"en": "Probe"}}},
		},
	})
	p := &handlerAssistProvider{}
	extension.RegisterAssistProvider("h.ext.assist2", p)
	t.Cleanup(func() { extension.RegisterAssistProvider("h.ext.assist2", nil) })

	admin := &model.User{ID: 1, Username: "admin", Role: model.Roles{model.ADMIN}}

	if w := assistChain(admin, "h.ext.assist2", "nope"); respCode(t, w) != 404 {
		t.Fatalf("undeclared assist form must be 404, got %d: %s", respCode(t, w), w.Body.String())
	}
	if p.called != "" {
		t.Fatalf("provider must not be invoked for an undeclared form, got called=%q", p.called)
	}
}

// handlerPrincipalAction is an in-process ext.Action double resolved through
// extension.SetActionFactory, used only to observe the ext.Principal
// InvokeExtensionAction's HTTP handler threads down to the gRPC leg's Invoke
// call. RegisterActionInvoker's in-process ActionInvoker seam (handlerInvoker
// above) never carries a principal by design (mirrors AssistProvider), so
// pinning principal-threading needs this gRPC-shaped seam instead.
type handlerPrincipalAction struct{ gotPrincipal ext.Principal }

func (f *handlerPrincipalAction) Manifest() ext.Manifest               { return ext.Manifest{ID: "unused"} }
func (f *handlerPrincipalAction) Init(context.Context, ext.Host) error { return nil }
func (f *handlerPrincipalAction) Shutdown(context.Context) error       { return nil }
func (f *handlerPrincipalAction) Invoke(_ context.Context, p ext.Principal, actionID string, args map[string]any) (any, error) {
	f.gotPrincipal = p
	return map[string]any{"ran": actionID}, nil
}

// TestInvokeActionHandlerThreadsCallerPrincipal pins item 5 (principal
// threading): the HTTP handler must resolve the request's authenticated user
// and pass it down as the ext.Principal InvokeExtensionAction forwards to
// Invoke — not the empty Principal.
func TestInvokeActionHandlerThreadsCallerPrincipal(t *testing.T) {
	m := ext.Manifest{
		ID:    "h.ext.actprin",
		Kinds: []ext.Kind{ext.KindAction},
		Contributions: ext.Contributions{
			MenuActions: []ext.MenuAction{{ID: "run", Label: map[string]string{"en": "Run"}, Target: "global"}},
		},
	}
	if err := extension.DefaultRegistry.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(m.ID) })

	fake := &handlerPrincipalAction{}
	extension.SetActionFactory(func(e *extension.Entry) (ext.Action, func(), error) {
		return fake, func() {}, nil
	})
	t.Cleanup(func() { extension.SetActionFactory(nil) })

	admin := &model.User{ID: 1, Username: "alice", Role: model.Roles{model.ADMIN}}
	if w := actionChain(admin, "h.ext.actprin", "run"); respCode(t, w) != 200 {
		t.Fatalf("code=%d body=%s", respCode(t, w), w.Body.String())
	}
	if fake.gotPrincipal != ext.Principal("alice") {
		t.Fatalf("principal = %q, want the request's authenticated username", fake.gotPrincipal)
	}
}

// handlerPrincipalAssist mirrors handlerPrincipalAction one leg over: an
// in-process ext.Assist double resolved through extension.SetAssistFactory,
// used to observe the ext.Principal SubmitExtensionAssist's HTTP handler
// threads down.
type handlerPrincipalAssist struct{ gotPrincipal ext.Principal }

func (f *handlerPrincipalAssist) Assist(_ context.Context, p ext.Principal, formID string, input map[string]any) (any, error) {
	f.gotPrincipal = p
	return map[string]any{"ran": formID}, nil
}

// TestSubmitAssistHandlerThreadsCallerPrincipal is
// TestInvokeActionHandlerThreadsCallerPrincipal one route over: pins that
// SubmitExtensionAssist's HTTP handler resolves the request's authenticated
// user and threads it down as the ext.Principal SubmitAssist forwards to
// Assist — not the empty Principal (the pre-item-5 behavior).
func TestSubmitAssistHandlerThreadsCallerPrincipal(t *testing.T) {
	m := ext.Manifest{
		ID:    "h.ext.assistprin",
		Kinds: []ext.Kind{ext.KindAction},
		Contributions: ext.Contributions{
			AssistForms: []ext.AssistForm{{ID: "probe", Title: map[string]string{"en": "Probe"}}},
		},
	}
	if err := extension.DefaultRegistry.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(m.ID) })

	fake := &handlerPrincipalAssist{}
	extension.SetAssistFactory(func(e *extension.Entry) (ext.Assist, func(), error) {
		return fake, func() {}, nil
	})
	t.Cleanup(func() { extension.SetAssistFactory(nil) })

	admin := &model.User{ID: 1, Username: "carol", Role: model.Roles{model.ADMIN}}
	if w := assistChain(admin, "h.ext.assistprin", "probe"); respCode(t, w) != 200 {
		t.Fatalf("code=%d body=%s", respCode(t, w), w.Body.String())
	}
	if fake.gotPrincipal != ext.Principal("carol") {
		t.Fatalf("principal = %q, want the request's authenticated username", fake.gotPrincipal)
	}
}

func TestInvokeActionHandlerNonAdminRejected(t *testing.T) {
	registerEnabledExtension(t, ext.Manifest{
		ID:    "h.ext.act2",
		Kinds: []ext.Kind{ext.KindAction},
		Contributions: ext.Contributions{
			MenuActions: []ext.MenuAction{{ID: "run", Label: map[string]string{"en": "Run"}, Target: "global"}},
		},
	})
	nonAdmin := &model.User{ID: 7, Username: "bob", Role: model.Roles{model.GENERAL}}
	if w := actionChain(nonAdmin, "h.ext.act2", "run"); respCode(t, w) != 403 {
		t.Fatalf("non-admin invoking an action must be 403, got %d: %s", respCode(t, w), w.Body.String())
	}
}

func TestWizardHandlersStartAndNext(t *testing.T) {
	registerEnabledExtension(t, ext.Manifest{
		ID:    "h.ext.wiz",
		Kinds: []ext.Kind{ext.KindStorageDriver},
		Wizard: ext.Wizard{Steps: []ext.WizardStep{
			{ID: "welcome", Type: ext.WizardInfo},
			{ID: "config", Type: ext.WizardForm, Schema: &ext.JSONSchema{
				Required:   []string{"name"},
				Properties: map[string]*ext.JSONSchema{"name": {Type: "string"}},
			}},
			{ID: "done", Type: ext.WizardDone},
		}},
	})

	// start
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "h.ext.wiz"}}
	c.Request = httptest.NewRequest("POST", "/api/v4/extensions/h.ext.wiz/wizard/start", nil)
	StartExtensionWizard(c)
	var start struct {
		Code int                      `json:"code"`
		Data extension.WizardStepView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil {
		t.Fatalf("decode start: %v (%s)", err, w.Body.String())
	}
	if start.Data.Step == nil || start.Data.Step.ID != "welcome" || start.Data.SessionID == "" {
		t.Fatalf("start view wrong: %+v", start.Data)
	}
	sid := start.Data.SessionID

	// next over the info step -> form
	next := func(body string) extension.WizardStepView {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "h.ext.wiz"}, {Key: "session", Value: sid}}
		req := httptest.NewRequest("POST", "/api/v4/extensions/h.ext.wiz/wizard/"+sid+"/next", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		c.Request = req
		ExtensionWizardNext(c)
		var resp struct {
			Data extension.WizardStepView `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode next: %v (%s)", err, w.Body.String())
		}
		return resp.Data
	}

	if v := next(""); v.Step == nil || v.Step.ID != "config" {
		t.Fatalf("after info step: %+v", v)
	}
	if v := next(`{"name":"alice"}`); !v.Done {
		t.Fatalf("after form step, expected done: %+v", v)
	}
}

// httpStubAuthProvider is an in-process ext.AuthProvider double for the HTTP
// handler test below (WS-8): v4 ships no real network drive/OAuth plugin, and
// the actual token-exchange-over-gRPC round trip is covered separately by
// internal/extension/grpc's TestAuthProviderGRPCRoundTrip. This test's job is
// proving the HTTP surface — StartExtensionWizard, the oauth callback route,
// and ExtensionWizardNext — wired together correctly end to end.
type httpStubAuthProvider struct{ id string }

func (p *httpStubAuthProvider) Manifest() ext.Manifest {
	return ext.Manifest{ID: p.id, Kinds: []ext.Kind{ext.KindAuthProvider}}
}
func (p *httpStubAuthProvider) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *httpStubAuthProvider) Shutdown(ctx context.Context) error         { return nil }

func (p *httpStubAuthProvider) AuthURL(ctx context.Context, req ext.AuthURLRequest) (string, error) {
	return "https://vendor.example.com/authorize?state=" + req.State + "&redirect_uri=" + req.RedirectURI, nil
}

func (p *httpStubAuthProvider) Exchange(ctx context.Context, req ext.ExchangeRequest) (ext.Token, error) {
	return ext.Token{AccessToken: "at-" + req.Code, RefreshToken: "rt-1"}, nil
}

func (p *httpStubAuthProvider) Refresh(ctx context.Context, refreshToken string) (ext.Token, error) {
	return ext.Token{AccessToken: "at-refreshed"}, nil
}

// TestWizardOAuthStepEndToEndThroughHTTP drives the whole WS-8 mechanism
// through the actual HTTP handlers: start the install wizard, land on an
// oauth step and receive an AuthURL carrying a state, hit the anonymous oauth
// callback route exactly as the vendor's browser redirect would, then confirm
// wizard/next advances to done with the exchanged token in hand.
func TestWizardOAuthStepEndToEndThroughHTTP(t *testing.T) {
	const providerID = "h.ext.authprovider"
	extension.RegisterAuthProviderInstance(providerID, &httpStubAuthProvider{id: providerID})
	t.Cleanup(func() { extension.RegisterAuthProviderInstance(providerID, nil) })
	// RegisterAuthProviderInstance is the in-process/test-double seam
	// (bypasses the registry's byID map entirely, mirroring
	// RegisterAuthNMethodInstance) — GetAuthProvider resolves it directly. But
	// a wizard oauth step's Provider is now ALSO validated at registration
	// (Register/RegisterPluginMode/Enable all call validateWizardOAuthProviders,
	// internal/extension/wizard.go) against the REGISTRY's DeclaresKind, which
	// only sees a real Entry — so this double additionally needs a plain
	// Kind=AuthProvider Entry for the check to find, exactly like a real
	// installed AuthProvider plugin (registered via RegisterPluginMode) would
	// already have.
	registerEnabledExtension(t, ext.Manifest{
		ID:    providerID,
		Kinds: []ext.Kind{ext.KindAuthProvider},
	})

	registerEnabledExtension(t, ext.Manifest{
		ID:    "h.ext.oauthwiz",
		Kinds: []ext.Kind{ext.KindStorageDriver},
		Wizard: ext.Wizard{Steps: []ext.WizardStep{
			{ID: "oauth", Type: ext.WizardOAuth, Provider: providerID, Scopes: []string{"read"}},
			{ID: "done", Type: ext.WizardDone},
		}},
	})

	gin.SetMode(gin.TestMode)

	// start
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "h.ext.oauthwiz"}}
	c.Request = httptest.NewRequest("POST", "/api/v4/extensions/h.ext.oauthwiz/wizard/start", nil)
	StartExtensionWizard(c)
	var start struct {
		Data extension.WizardStepView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil {
		t.Fatalf("decode start: %v (%s)", err, w.Body.String())
	}
	if start.Data.Step == nil || start.Data.Step.ID != "oauth" || start.Data.AuthURL == "" {
		t.Fatalf("expected an oauth step with an AuthURL: %+v", start.Data)
	}
	sid := start.Data.SessionID
	u, err := url.Parse(start.Data.AuthURL)
	if err != nil {
		t.Fatalf("AuthURL unparseable: %v", err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("AuthURL must carry a state: %q", start.Data.AuthURL)
	}

	// vendor redirect -> anonymous oauth callback route
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v4/extensions/oauth/callback?code=code-1&state="+url.QueryEscape(state), nil)
	ExtensionOAuthCallback(c)
	if w.Code != 200 {
		t.Fatalf("callback status = %d body=%s", w.Code, w.Body.String())
	}
	var cb struct {
		Data extension.WizardStepView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cb); err != nil {
		t.Fatalf("decode callback: %v (%s)", err, w.Body.String())
	}
	if cb.Data.Answers == nil || cb.Data.Answers["token"] == nil {
		t.Fatalf("callback response must carry the exchanged token: %+v", cb.Data)
	}

	// a replayed/forged state must be rejected with 400
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v4/extensions/oauth/callback?code=code-2&state="+url.QueryEscape(state), nil)
	ExtensionOAuthCallback(c)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("replayed state must be rejected with 400, got %d: %s", got, w.Body.String())
	}

	// main tab resumes the wizard: next() advances past the completed oauth step
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "h.ext.oauthwiz"}, {Key: "session", Value: sid}}
	c.Request = httptest.NewRequest("POST", "/api/v4/extensions/h.ext.oauthwiz/wizard/"+sid+"/next", nil)
	ExtensionWizardNext(c)
	var next struct {
		Data extension.WizardStepView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &next); err != nil {
		t.Fatalf("decode next: %v (%s)", err, w.Body.String())
	}
	if !next.Data.Done {
		t.Fatalf("expected wizard done after oauth completion: %+v", next.Data)
	}
	tok, ok := next.Data.Answers["token"].(map[string]any)
	if !ok || tok["access_token"] != "at-code-1" {
		t.Fatalf("done answers must carry the exchanged token: %+v", next.Data.Answers)
	}
}

// TestScopedTokenReadsOnlyInScope drives the full round trip: issue a scoped
// token within the extension's declared paths, authenticate it into a
// restricted principal, and prove op.Authorize confines that principal to
// exactly the declared subtree — reads inside pass, reads outside are denied.
func TestScopedTokenReadsOnlyInScope(t *testing.T) {
	tokenstore.Init(tokenstore.NewSQLiteStore())
	registerEnabledExtension(t, ext.Manifest{
		ID:          "h.ext.store",
		Kinds:       []ext.Kind{ext.KindStorageDriver},
		Permissions: ext.Permissions{StorageTokens: []string{"/data"}},
	})

	tok, err := extension.IssueScopedToken(extension.DefaultRegistry, "h.ext.store",
		[]extension.Scope{{Path: "/data/reports"}}, time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	user, err := extension.AuthenticateScopedToken(tok)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	ctx := context.Background()
	// inside scope: allowed
	if err := op.Authorize(ctx, user, model.ActRead, op.AuthzRequest{Path: "/data/reports/q1.csv"}); err != nil {
		t.Fatalf("read inside scope must be allowed: %v", err)
	}
	// outside scope: denied
	if err := op.Authorize(ctx, user, model.ActRead, op.AuthzRequest{Path: "/secret/keys"}); err == nil {
		t.Fatal("read outside scope must be denied")
	}
	// a read-only scope must not permit writes even inside scope
	if err := op.Authorize(ctx, user, model.ActWrite, op.AuthzRequest{Path: "/data/reports/q1.csv"}); err == nil {
		t.Fatal("read-only scope must not permit writes")
	}

	// revoked token no longer authenticates
	if err := extension.RevokeScopedToken(tok); err != nil {
		t.Fatal(err)
	}
	if _, err := extension.AuthenticateScopedToken(tok); err == nil {
		t.Fatal("revoked scoped token must not authenticate")
	}
}
