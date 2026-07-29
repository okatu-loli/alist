package handles

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/gin-gonic/gin"
)

// fakeBuiltinBundle is a minimal ext.FeatureBundle used to register a
// throwaway built-in into DefaultRegistry for the extension-manager tests. It
// implements only the base Extension + FeatureBundle contract (no extra
// extension-point interfaces), so RegisterBuiltinFeatureBundle inserts a plain
// enabled, builtin==true entry without wiring any F1/F2 dispatch legs.
type fakeBuiltinBundle struct {
	id string
	// warn is an optional DisableWarning LangMap surfaced through the manifest,
	// letting a test register a built-in that declares a disable consequence.
	warn map[string]string
	// extraKinds are additional Kinds appended after KindFeatureBundle in the
	// manifest's declared Kinds list, letting a test register a multi-Kind
	// built-in (e.g. + KindBackgroundService) without a dedicated FeatureBundle
	// expansion — RegisterBuiltinFeatureBundle still indexes the entry under
	// its PRIMARY Kind (FeatureBundle) only.
	extraKinds []ext.Kind
}

func (b *fakeBuiltinBundle) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:             b.id,
		Name:           map[string]string{"en": "Fake Builtin", "zh": "假内置"},
		Kinds:          append([]ext.Kind{ext.KindFeatureBundle}, b.extraKinds...),
		DisableWarning: b.warn,
	}
}
func (b *fakeBuiltinBundle) Init(ctx context.Context, host ext.Host) error { return nil }
func (b *fakeBuiltinBundle) Shutdown(ctx context.Context) error            { return nil }
func (b *fakeBuiltinBundle) Bundle() ext.BundleSpec                        { return ext.BundleSpec{} }

// registerFakeBuiltin registers a throwaway built-in into DefaultRegistry and
// removes it on cleanup, so the extension-manager tests have a known built-in id
// to list/enable/disable without depending on which real built-ins happen to be
// booted in the test binary. It also clears any disabled-set persistence the
// test toggled, keeping DefaultBuiltinManager's in-memory store clean for other
// tests.
func registerFakeBuiltin(t *testing.T, id string) {
	t.Helper()
	registerFakeBuiltinWithWarning(t, id, nil)
}

// registerFakeBuiltinWithWarning is registerFakeBuiltin plus an optional
// DisableWarning LangMap, so a test can assert the manifest field flows through
// to BuiltinExtensionDTO.disable_warning.
func registerFakeBuiltinWithWarning(t *testing.T, id string, warn map[string]string) {
	t.Helper()
	if err := extension.RegisterBuiltinFeatureBundle(extension.DefaultRegistry, &fakeBuiltinBundle{id: id, warn: warn}); err != nil {
		t.Fatalf("register fake builtin: %v", err)
	}
	t.Cleanup(func() {
		// Re-enable then deregister so a test that left it disabled does not leak
		// the id into the persisted disabled set for the next test.
		_ = extension.EnableBuiltin(id)
		extension.DefaultRegistry.Deregister(id)
	})
}

// registerFakeBuiltinWithKinds registers a built-in whose Manifest declares
// KindFeatureBundle plus extraKinds (e.g. KindBackgroundService), still
// indexed under the primary Kind (FeatureBundle) only.
func registerFakeBuiltinWithKinds(t *testing.T, id string, extraKinds ...ext.Kind) {
	t.Helper()
	if err := extension.RegisterBuiltinFeatureBundle(extension.DefaultRegistry, &fakeBuiltinBundle{id: id, extraKinds: extraKinds}); err != nil {
		t.Fatalf("register fake builtin: %v", err)
	}
	t.Cleanup(func() {
		_ = extension.EnableBuiltin(id)
		extension.DefaultRegistry.Deregister(id)
	})
}

func extAdmin() *model.User {
	return &model.User{ID: 1, Username: "admin", Role: model.Roles{model.ADMIN}}
}

// listManagedExtensions drives ListManagedExtensions and decodes the response.
func listManagedExtensions(t *testing.T) ManagedExtensionsResp {
	t.Helper()
	setupPluginGuardEnv(t) // point DefaultManager at a scratch dir
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, extAdmin())
	c.Request = httptest.NewRequest("GET", "/api/v4/admin/extensions", nil)
	ListManagedExtensions(c)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("list: expected 200, got %d: %s", code, w.Body.String())
	}
	var resp struct {
		Data ManagedExtensionsResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v (%s)", err, w.Body.String())
	}
	return resp.Data
}

func findBuiltin(list []BuiltinExtensionDTO, id string) (BuiltinExtensionDTO, bool) {
	for _, b := range list {
		if b.ID == id {
			return b, true
		}
	}
	return BuiltinExtensionDTO{}, false
}

// TestListManagedExtensionsIncludesBuiltins verifies the unified list surfaces a
// registered built-in with builtin==true and its i18n name/kind, and that the
// plugins section is present (non-nil slice).
func TestListManagedExtensionsIncludesBuiltins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const id = "test.builtin.list"
	registerFakeBuiltin(t, id)

	data := listManagedExtensions(t)
	b, ok := findBuiltin(data.Builtins, id)
	if !ok {
		t.Fatalf("expected built-in %q in list, got %+v", id, data.Builtins)
	}
	if !b.Builtin {
		t.Fatalf("expected builtin==true, got %+v", b)
	}
	if !b.Enabled {
		t.Fatalf("expected freshly-registered built-in enabled, got %+v", b)
	}
	if b.Name["en"] != "Fake Builtin" {
		t.Fatalf("expected i18n name carried through, got %+v", b.Name)
	}
	if b.Kind != string(ext.KindFeatureBundle) {
		t.Fatalf("expected kind %q, got %q", ext.KindFeatureBundle, b.Kind)
	}
	if data.Plugins == nil {
		t.Fatal("expected a (possibly empty) plugins section, got nil")
	}
}

// TestListManagedExtensionsSurfacesDisableWarning verifies the declarative
// DisableWarning manifest field flows through to BuiltinExtensionDTO: a built-in
// that declares a consequence surfaces it (both languages), while one that does
// not leaves the field empty — replacing the frontend's hardcoded confirm list.
func TestListManagedExtensionsSurfacesDisableWarning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const withID = "test.builtin.warn"
	const withoutID = "test.builtin.nowarn"
	warn := map[string]string{
		"en": "Disabling this immediately invalidates all public share links",
		"zh": "关闭后所有公开分享链接将立即失效",
	}
	registerFakeBuiltinWithWarning(t, withID, warn)
	registerFakeBuiltin(t, withoutID)

	data := listManagedExtensions(t)

	b, ok := findBuiltin(data.Builtins, withID)
	if !ok {
		t.Fatalf("expected built-in %q in list, got %+v", withID, data.Builtins)
	}
	if b.DisableWarning["en"] != warn["en"] || b.DisableWarning["zh"] != warn["zh"] {
		t.Fatalf("expected disable_warning carried through with en+zh, got %+v", b.DisableWarning)
	}

	nb, ok := findBuiltin(data.Builtins, withoutID)
	if !ok {
		t.Fatalf("expected built-in %q in list, got %+v", withoutID, data.Builtins)
	}
	if len(nb.DisableWarning) != 0 {
		t.Fatalf("expected empty disable_warning for a built-in without a consequence, got %+v", nb.DisableWarning)
	}
}

// TestListManagedExtensionsHasStatusMultiKindManifestUsesManifestTruthSource
// mirrors TestInvokeActionMultiKindManifestUsesManifestTruthSource
// (internal/extension/actions_test.go): a built-in registered under primary
// Kind=FeatureBundle whose Manifest additionally declares BackgroundService
// (Kinds: [FeatureBundle, BackgroundService]) never gets an
// extraKinds(BackgroundService) entry — extraKinds is populated solely by
// FeatureBundle indexing onto APIRoute/EventHook/ScheduledJob/FileDecorator,
// never BackgroundService itself — so e.HasKind(BackgroundService) alone
// would wrongly render has_status=false despite the built-in declaring the
// service. DeclaresKind must report has_status=true.
func TestListManagedExtensionsHasStatusMultiKindManifestUsesManifestTruthSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const id = "test.builtin.multikind.status"
	registerFakeBuiltinWithKinds(t, id, ext.KindBackgroundService)

	data := listManagedExtensions(t)
	b, ok := findBuiltin(data.Builtins, id)
	if !ok {
		t.Fatalf("expected built-in %q in list, got %+v", id, data.Builtins)
	}
	if !b.HasStatus {
		t.Fatalf("expected has_status=true for a Manifest declaring BackgroundService alongside its primary Kind, got %+v", b)
	}
}

// TestDisableThenEnableBuiltin toggles a built-in off then on through the
// endpoints and confirms the list reflects each state — a disabled built-in is
// still listed (enabled==false), and re-enabling flips it back.
func TestDisableThenEnableBuiltin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const id = "test.builtin.toggle"
	registerFakeBuiltin(t, id)

	// disable
	callBuiltinToggle(t, "disable", id, 200)
	data := listManagedExtensions(t)
	b, ok := findBuiltin(data.Builtins, id)
	if !ok {
		t.Fatalf("disabled built-in must still be listed, got %+v", data.Builtins)
	}
	if b.Enabled {
		t.Fatalf("expected enabled==false after disable, got %+v", b)
	}

	// enable back
	callBuiltinToggle(t, "enable", id, 200)
	data = listManagedExtensions(t)
	b, _ = findBuiltin(data.Builtins, id)
	if !b.Enabled {
		t.Fatalf("expected enabled==true after re-enable, got %+v", b)
	}
}

// TestBuiltinEndpointsRejectNonBuiltin confirms the built-in enable/disable
// endpoints reject an id that is not a registered built-in (a disk plugin id or
// an unknown id) with 400 — disk plugins must go through /admin/plugins, and
// this endpoint never mutates them.
func TestBuiltinEndpointsRejectNonBuiltin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	callBuiltinToggle(t, "disable", "does.not.exist", 400)
	callBuiltinToggle(t, "enable", "does.not.exist", 400)
}

// TestManagedExtensionsNonAdminForbidden wires the real RequireAdmin gate in
// front of the endpoints (mirroring the router) and confirms a non-admin
// principal is refused with 403 on both the list and the toggle routes.
func TestManagedExtensionsNonAdminForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	nonAdmin := &model.User{ID: 7, Username: "bob", Role: model.Roles{model.GENERAL}}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(ctxkey.User, nonAdmin) })
	exts := r.Group("/api/v4/admin/extensions", middlewares.RequireAdmin)
	exts.GET("", ListManagedExtensions)
	exts.POST("/:id/enable", EnableBuiltinExtension)
	exts.POST("/:id/disable", DisableBuiltinExtension)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v4/admin/extensions"},
		{"POST", "/api/v4/admin/extensions/whatever/enable"},
		{"POST", "/api/v4/admin/extensions/whatever/disable"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		r.ServeHTTP(w, req)
		if code := respCode(t, w); code != 403 {
			t.Fatalf("non-admin %s %s: expected 403, got %d: %s", tc.method, tc.path, code, w.Body.String())
		}
	}
}

// callBuiltinToggle drives EnableBuiltinExtension/DisableBuiltinExtension for id
// and asserts the response code.
func callBuiltinToggle(t *testing.T, verb, id string, want int) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, extAdmin())
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/extensions/"+id+"/"+verb, nil)
	if verb == "disable" {
		DisableBuiltinExtension(c)
	} else {
		EnableBuiltinExtension(c)
	}
	if code := respCode(t, w); code != want {
		t.Fatalf("%s %q: expected %d, got %d: %s", verb, id, want, code, w.Body.String())
	}
}
