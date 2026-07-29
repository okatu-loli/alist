package handles

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// meEntry mirrors the typed permission_entries the frontend consumes: fs rows
// carry path+recursive, system rows carry domain, both carry actions+effect.
type meEntry struct {
	ResourceType string   `json:"resource_type"`
	Path         string   `json:"path"`
	Domain       string   `json:"domain"`
	Actions      []string `json:"actions"`
	Effect       string   `json:"effect"`
	Recursive    *bool    `json:"recursive"`
}

// meData drives CurrentUser and returns the raw data map so both the new
// permission_entries and the legacy fields can be inspected in the same shot.
func meData(t *testing.T, u *model.User) map[string]json.RawMessage {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/me", nil)
	c.Set(ctxkey.User, u)
	CurrentUser(c)

	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, w.Body.String())
	}
	if env.Code != 200 {
		t.Fatalf("/me code=%d body=%s", env.Code, w.Body.String())
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v; data=%s", err, string(env.Data))
	}
	return data
}

func meEntries(t *testing.T, data map[string]json.RawMessage) []meEntry {
	t.Helper()
	raw, ok := data["permission_entries"]
	if !ok {
		t.Fatalf("/me response is missing permission_entries; keys=%v", keysOf(data))
	}
	var entries []meEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("unmarshal permission_entries: %v; raw=%s", err, string(raw))
	}
	return entries
}

func keysOf(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// meOperator builds a non-admin operator whose single role carries the given
// permission rows. Rows are written at the db layer (bypassing op-level catalog
// validation) so an unregistered action can be seeded for the opaque test.
func meOperator(t *testing.T, rows ...*model.Permission) *model.User {
	t.Helper()
	r := createNonReservedRole(t, "me-op-role")
	for _, p := range rows {
		if err := db.CreatePermission(p); err != nil {
			t.Fatal(err)
		}
		pp := p
		t.Cleanup(func() { _ = db.DeletePermissionById(pp.ID) })
		if err := db.AddRolePermission(r.ID, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	op.InvalidateRolePolicyForTest(r.ID)
	return &model.User{ID: 7777, Username: "me-op", Role: model.Roles{int(r.ID)}}
}

// TestMeShowsSystemGrants: a storage-manager operator's /me must surface the
// system storage grant that the legacy int32 bitmask could never represent —
// and must keep every legacy field the running frontend still reads.
func TestMeShowsSystemGrants(t *testing.T) {
	u := meOperator(t,
		&model.Permission{Name: "me-fs", ResourceType: model.ResourceFS, Path: "/media",
			Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
		&model.Permission{Name: "me-sys", ResourceType: model.ResourceSystem, Path: "storage",
			Actions: model.Actions{model.SystemAction("storage", "view"), model.SystemAction("storage", "manage")},
			Effect:  model.EffectAllow},
	)
	data := meData(t, u)

	var sys *meEntry
	for i := range meEntries(t, data) {
		e := meEntries(t, data)[i]
		if e.ResourceType == model.ResourceSystem && e.Domain == "storage" {
			sys = &e
			break
		}
	}
	if sys == nil {
		t.Fatalf("permission_entries missing system storage entry; got %+v", meEntries(t, data))
	}
	if sys.Effect != model.EffectAllow {
		t.Errorf("system entry effect = %q, want allow", sys.Effect)
	}
	if !containsStr(sys.Actions, string(model.SystemAction("storage", "view"))) ||
		!containsStr(sys.Actions, string(model.SystemAction("storage", "manage"))) {
		t.Errorf("system entry actions = %v, want storage view+manage", sys.Actions)
	}

	// regression guard: legacy fields the frontend still consumes must remain.
	for _, k := range []string{"id", "username", "role", "role_names",
		"permission", "permissions", "disabled", "otp", "sso_id"} {
		if _, ok := data[k]; !ok {
			t.Errorf("legacy /me field %q missing; response must stay additive", k)
		}
	}
}

// TestMeFsEntriesTyped: fs rows render as {resource_type:fs, path, actions,
// effect, recursive} rather than a bitmask.
func TestMeFsEntriesTyped(t *testing.T) {
	u := meOperator(t,
		&model.Permission{Name: "me-fs", ResourceType: model.ResourceFS, Path: "/media",
			Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
	)
	data := meData(t, u)

	var fs *meEntry
	for i, e := range meEntries(t, data) {
		if e.ResourceType == model.ResourceFS && e.Path == "/media" {
			es := meEntries(t, data)
			fs = &es[i]
			break
		}
	}
	if fs == nil {
		t.Fatalf("permission_entries missing fs /media entry; got %+v", meEntries(t, data))
	}
	if fs.Recursive == nil || !*fs.Recursive {
		t.Errorf("fs entry recursive = %v, want true and present", fs.Recursive)
	}
	if fs.Effect != model.EffectAllow {
		t.Errorf("fs entry effect = %q, want allow", fs.Effect)
	}
	if !containsStr(fs.Actions, string(model.ActRead)) {
		t.Errorf("fs entry actions = %v, want read", fs.Actions)
	}
	if fs.Domain != "" {
		t.Errorf("fs entry must not carry a domain, got %q", fs.Domain)
	}
}

// TestMeUnknownActionOpaque: an action with no catalog descriptor still passes
// through in the entry's actions[] — never dropped, never an error.
func TestMeUnknownActionOpaque(t *testing.T) {
	const opaque = "system.mystery.doit"
	u := meOperator(t,
		&model.Permission{Name: "me-opaque", ResourceType: model.ResourceSystem, Path: "mystery",
			Actions: model.Actions{model.Action(opaque)}, Effect: model.EffectAllow},
	)
	data := meData(t, u)

	found := false
	for _, e := range meEntries(t, data) {
		if containsStr(e.Actions, opaque) {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown action %q was dropped from permission_entries; got %+v", opaque, meEntries(t, data))
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
