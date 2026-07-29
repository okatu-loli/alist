package handles

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/gin-gonic/gin"
)

// Labels handler security tests (design §3/§6). The gates are authorization-
// sensitive, so each is driven end to end: apply/remove must require ActWrite on
// the target path, of/files must never reveal a path the caller cannot ActRead,
// and the vocabulary CRUD is admin-only (AuthAdmin, driven through the real
// middleware). LabelFiles is the load-bearing one — it lists every node a label
// binds, so it must per-path ActRead-filter or it leaks unreadable files.

// labelFixtureSeq gives each fixture a globally unique storage mount / label
// name / user id, so tests sharing the package's one in-memory DB never collide
// on the storages.mount_path or labels.name unique indexes.
var labelFixtureSeq int64

// setupLabelFixture builds an isolated Local storage mounted at a unique /lblN
// with a "vis" subtree the returned non-admin user may read+write and a "sec"
// subtree it has no rights on at all, plus a fresh vocabulary entry. The
// visible/secret split is what the ActWrite (apply/remove) and ActRead
// (of/files) gates are asserted against.
func setupLabelFixture(t *testing.T) (*model.User, *model.Label, string) {
	t.Helper()
	n := atomic.AddInt64(&labelFixtureSeq, 1)
	mount := fmt.Sprintf("/lbl%d", n)

	dir := t.TempDir()
	for _, d := range []string{"vis", "sec"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "vis", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sec", "s.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	sid, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	// A role scoped read+write on <mount>/vis only (bit 3 == ActWrite; ActRead is
	// implicit for every scope-covered path). <mount>/sec is left uncovered, so
	// the user has neither read nor write there.
	r := createScopedRole(t, fmt.Sprintf("lbl-role-%d", n), []model.PermissionEntry{
		{Path: mount + "/vis", Permission: 1 << 3},
	})
	user := &model.User{ID: 30000 + uint(n), Username: fmt.Sprintf("lbluser%d", n), Role: model.Roles{int(r.ID)}}

	red := &model.Label{Name: fmt.Sprintf("red-%d", n), Color: "#ff0000"}
	if err := op.CreateLabel(red); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteLabel(red.ID) })
	return user, red, mount
}

// createScopedRole creates a role carrying the given permission scopes whose id
// is guaranteed greater than model.ADMIN. Role ids double as identity (GUEST=1,
// ADMIN=2) and the in-memory test DB never seeds the built-ins, so an early role
// could otherwise land on id 2 and make its user read as admin — masking the
// very denials these tests assert. Auto-increment never reuses ids, so this
// settles in <=2 tries.
func createScopedRole(t *testing.T, base string, scopes []model.PermissionEntry) *model.Role {
	t.Helper()
	for i := 0; ; i++ {
		r := &model.Role{Name: fmt.Sprintf("%s-%d", base, i), PermissionScopes: scopes}
		if err := op.CreateRole(r); err != nil {
			t.Fatal(err)
		}
		if r.ID > model.ADMIN {
			t.Cleanup(func() { _ = op.DeleteRole(r.ID) })
			return r
		}
		_ = op.DeleteRole(r.ID)
	}
}

func adminUser(t *testing.T) *model.User {
	t.Helper()
	n := atomic.AddInt64(&labelFixtureSeq, 1)
	return &model.User{ID: 50000 + uint(n), Username: fmt.Sprintf("lbladmin%d", n), Role: model.Roles{model.ADMIN}}
}

func nonAdminUser(t *testing.T) *model.User {
	t.Helper()
	n := atomic.AddInt64(&labelFixtureSeq, 1)
	return &model.User{ID: 60000 + uint(n), Username: fmt.Sprintf("lblplain%d", n), Role: model.Roles{model.GENERAL}}
}

// driveJSON runs a handler with a JSON body and the caller in context.
func driveJSON(u *model.User, method, body string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/x", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

// driveQuery runs a handler whose input is a ?path= query param.
func driveQuery(u *model.User, path string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x?path="+url.QueryEscape(path), nil)
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

// driveParam runs a handler whose input is an :id path param.
func driveParam(u *model.User, id string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

func parseLabelResp(t *testing.T, w *httptest.ResponseRecorder) *model.Label {
	t.Helper()
	var env struct {
		Data model.Label `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse label resp: %v (%s)", err, w.Body.String())
	}
	return &env.Data
}

// TestLabelApplyAndRemoveAuthz pins the ActWrite gate on both mutations: a
// caller without write on the target path can neither apply nor remove a label,
// and — just as important — a denied call must not have touched the binding.
func TestLabelApplyAndRemoveAuthz(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("apply-no-write-403", func(t *testing.T) {
		u, red, mount := setupLabelFixture(t)
		secPath := mount + "/sec/s.txt"
		w := driveJSON(u, "POST", fmt.Sprintf(`{"path":%q,"label_id":%d}`, secPath, red.ID), LabelApply)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("apply without write must be 403, got %d: %s", got, w.Body.String())
		}
		// A denied apply must not have bound anything either: a 403 that still
		// wrote the binding is not a fix.
		labels, err := op.LabelsOf(secPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(labels) != 0 {
			t.Fatalf("denied apply must not bind, got %v", labels)
		}
	})

	t.Run("apply-with-write-binds", func(t *testing.T) {
		u, red, mount := setupLabelFixture(t)
		p := mount + "/vis/a.txt"
		w := driveJSON(u, "POST", fmt.Sprintf(`{"path":%q,"label_id":%d}`, p, red.ID), LabelApply)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("apply with write must be 200, got %d: %s", got, w.Body.String())
		}
		labels, err := op.LabelsOf(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(labels) != 1 || labels[0].ID != red.ID {
			t.Fatalf("apply must bind the label, got %v", labels)
		}
	})

	t.Run("remove-no-write-403", func(t *testing.T) {
		u, red, mount := setupLabelFixture(t)
		secPath := mount + "/sec/s.txt"
		// Pre-bind directly (bypassing the authz gate) so there is a binding for a
		// bypassed remove to try to destroy.
		if err := op.ApplyLabel(context.Background(), secPath, red.ID); err != nil {
			t.Fatal(err)
		}
		w := driveJSON(u, "POST", fmt.Sprintf(`{"path":%q,"label_id":%d}`, secPath, red.ID), LabelRemove)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("remove without write must be 403, got %d: %s", got, w.Body.String())
		}
		labels, err := op.LabelsOf(secPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(labels) != 1 {
			t.Fatalf("denied remove must not unbind, got %v", labels)
		}
	})

	t.Run("remove-with-write-unbinds", func(t *testing.T) {
		u, red, mount := setupLabelFixture(t)
		p := mount + "/vis/a.txt"
		if err := op.ApplyLabel(context.Background(), p, red.ID); err != nil {
			t.Fatal(err)
		}
		w := driveJSON(u, "POST", fmt.Sprintf(`{"path":%q,"label_id":%d}`, p, red.ID), LabelRemove)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("remove with write must be 200, got %d: %s", got, w.Body.String())
		}
		labels, err := op.LabelsOf(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(labels) != 0 {
			t.Fatalf("remove must unbind, got %v", labels)
		}
	})
}

// TestLabelsOfAuthz pins the ActRead gate on reading a path's labels: no read,
// no labels (403); with read, the bound label is returned.
func TestLabelsOfAuthz(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("no-read-403", func(t *testing.T) {
		u, _, mount := setupLabelFixture(t)
		w := driveQuery(u, mount+"/sec/s.txt", LabelsOf)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("LabelsOf without read must be 403, got %d: %s", got, w.Body.String())
		}
	})

	t.Run("with-read-ok", func(t *testing.T) {
		u, red, mount := setupLabelFixture(t)
		p := mount + "/vis/a.txt"
		if err := op.ApplyLabel(context.Background(), p, red.ID); err != nil {
			t.Fatal(err)
		}
		w := driveQuery(u, p, LabelsOf)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("LabelsOf with read must be 200, got %d: %s", got, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), red.Name) {
			t.Fatalf("LabelsOf must return the bound label: %s", w.Body.String())
		}
	})
}

// TestLabelFilesHidesUnreadable is the privilege-escalation regression: the same
// label is bound to a file the caller may read and a file the caller may not,
// and the "files with this label" filter view must return only the readable one.
// Without the handler's per-node ActRead filter, the filter view would enumerate
// every file a label touches — a direct cross-tenant path disclosure.
func TestLabelFilesHidesUnreadable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, red, mount := setupLabelFixture(t)
	visPath := mount + "/vis/a.txt"
	secPath := mount + "/sec/s.txt"
	// Bind the same label to both a visible and a secret file (direct op, so the
	// bindings exist regardless of the caller's rights).
	if err := op.ApplyLabel(context.Background(), visPath, red.ID); err != nil {
		t.Fatal(err)
	}
	if err := op.ApplyLabel(context.Background(), secPath, red.ID); err != nil {
		t.Fatal(err)
	}

	w := driveParam(u, strconv.Itoa(int(red.ID)), LabelFiles)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("LabelFiles must be 200, got %d: %s", got, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, visPath) {
		t.Fatalf("LabelFiles must list the caller-visible file %q: %s", visPath, body)
	}
	if strings.Contains(body, secPath) {
		t.Fatalf("LabelFiles leaked an unreadable file %q (privilege escalation): %s", secPath, body)
	}
}

// TestLabelVocabularyAdminOnly pins the admin gate on vocabulary writes (spec
// §3): a non-admin is stopped by AuthAdmin before the handler runs, an admin
// succeeds, and deleting a label cascades its bindings.
func TestLabelVocabularyAdminOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("create-non-admin-403", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"nope","color":"#000000"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(ctxkey.User, nonAdminUser(t))
		// The vocabulary CRUD carries no handler-layer check; the admin gate is
		// the AuthAdmin backstop on the /admin group, so drive that.
		middlewares.AuthAdmin(c)
		if !c.IsAborted() {
			t.Fatal("non-admin must be stopped by AuthAdmin before LabelCreate runs")
		}
		if got := respCode(t, w); got != 403 {
			t.Fatalf("non-admin create want 403, got %d: %s", got, w.Body.String())
		}
	})

	t.Run("create-admin-ok-and-cascade-delete", func(t *testing.T) {
		_, _, mount := setupLabelFixture(t)
		admin := adminUser(t)
		n := atomic.AddInt64(&labelFixtureSeq, 1)
		name := fmt.Sprintf("teamblue-%d", n)

		// Admin creates a vocabulary entry through the gate.
		wc := httptest.NewRecorder()
		cc, _ := gin.CreateTestContext(wc)
		cc.Request = httptest.NewRequest("POST", "/x", strings.NewReader(fmt.Sprintf(`{"name":%q,"color":"#0000ff"}`, name)))
		cc.Request.Header.Set("Content-Type", "application/json")
		cc.Set(ctxkey.User, admin)
		middlewares.AuthAdmin(cc)
		if cc.IsAborted() {
			t.Fatal("admin must pass AuthAdmin")
		}
		LabelCreate(cc)
		if got := respCode(t, wc); got != 200 {
			t.Fatalf("admin create want 200, got %d: %s", got, wc.Body.String())
		}
		created := parseLabelResp(t, wc)
		t.Cleanup(func() { _ = op.DeleteLabel(created.ID) })

		// Bind it to a file and prove the binding exists before deletion.
		if err := op.ApplyLabel(context.Background(), mount+"/vis/a.txt", created.ID); err != nil {
			t.Fatal(err)
		}
		nodes, err := db.BindingsByLabel(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) == 0 {
			t.Fatal("precondition: a binding must exist before the cascade-delete")
		}

		// Admin deletes the label; its bindings cascade away.
		wd := httptest.NewRecorder()
		cd, _ := gin.CreateTestContext(wd)
		cd.Request = httptest.NewRequest("DELETE", "/x", nil)
		cd.Params = gin.Params{{Key: "id", Value: strconv.Itoa(int(created.ID))}}
		cd.Set(ctxkey.User, admin)
		middlewares.AuthAdmin(cd)
		if cd.IsAborted() {
			t.Fatal("admin must pass AuthAdmin")
		}
		LabelDelete(cd)
		if got := respCode(t, wd); got != 200 {
			t.Fatalf("admin delete want 200, got %d: %s", got, wd.Body.String())
		}
		after, err := db.BindingsByLabel(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != 0 {
			t.Fatalf("delete must cascade bindings, %d remain", len(after))
		}
	})
}

// TestLabelValidationRejected pins that the op-layer name/color validation is
// surfaced by the handler as a 400: an empty name and a non-#RRGGBB color are
// both refused (the color guard is what keeps a stored value out of a style
// attribute as an injection vector, spec §6).
func TestLabelValidationRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name string
		body string
	}{
		{"empty-name", `{"name":"   ","color":"#ffffff"}`},
		{"injection-color", `{"name":"validname","color":"red;evil"}`},
		{"short-hex-color", `{"name":"validname2","color":"#12"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/x", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			LabelCreate(c)
			if got := respCode(t, w); got != 400 {
				t.Fatalf("invalid label must be 400, got %d: %s", got, w.Body.String())
			}
		})
	}
}
