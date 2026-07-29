package handles

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// mkStorage mounts a Local storage rooted at dir and cleans it up.
func mkStorage(t *testing.T, mount, dir string) uint {
	t.Helper()
	id, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mount,
		Driver:    "Local",
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, dir),
	})
	if err != nil {
		t.Fatalf("create storage %s: %v", mount, err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	return id
}

// permUser builds a user whose sole role is bound to the given permissions, so
// its effective policy is exactly those rows. fsAllowAll grants every fs action
// recursively from "/"; withManage additionally binds system:permission manage.
func permUser(t *testing.T, name string, fsAllowAll, withManage bool, extra ...*model.Permission) *model.User {
	t.Helper()
	r := &model.Role{Name: name + "-role"}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })

	var perms []*model.Permission
	if fsAllowAll {
		perms = append(perms, &model.Permission{
			Name: name + "-fs", Path: "/", Origin: model.OriginManual,
			Actions: model.Actions{model.ActRead, model.ActWrite, model.ActMove, model.ActCopy, model.ActRename, model.ActRemove},
			Effect:  model.EffectAllow, Recursive: true,
		})
	}
	if withManage {
		perms = append(perms, &model.Permission{
			Name: name + "-mgr", ResourceType: model.ResourceSystem, Path: "permission", Origin: model.OriginManual,
			Actions: model.Actions{model.SystemAction("permission", "manage")}, Effect: model.EffectAllow,
		})
	}
	// The caller's own grant rows (fsAllowAll at "/", the system manage row)
	// stay nodeless — they match on Path. The `extra` rows are the manual
	// permissions a copy/detach acts on; route them through op.CreatePermission
	// so they are anchored to a node exactly as a real admin create would be
	// (node-based capture/clone only sees rows that carry a NodeID).
	base := perms
	for _, p := range base {
		if err := db.CreatePermission(p); err != nil {
			t.Fatal(err)
		}
		pid := p.ID
		t.Cleanup(func() { _ = db.DeletePermissionById(pid) })
		if err := db.AddRolePermission(r.ID, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range extra {
		if err := op.CreatePermission(p); err != nil {
			t.Fatal(err)
		}
		pid := p.ID
		t.Cleanup(func() { _ = db.DeletePermissionById(pid) })
		if err := db.AddRolePermission(r.ID, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	op.InvalidateRolePolicyForTest(r.ID)
	return &model.User{ID: 40000 + uint(len(name)), Username: name, Role: model.Roles{int(r.ID)}}
}

// callHandler drives a handler and returns the envelope's Code. alist always
// replies HTTP 200 and carries the real status in the JSON body's "code", so
// tests assert on that, not w.Code.
func callHandler(t *testing.T, h gin.HandlerFunc, u *model.User, path, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", path, bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.User, u)
	h(c)
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return env.Code, w.Body.String()
}

// TestFsMoveDetachRequiresManage: a caller with full fs rights but no
// system:permission manage is rejected 403 for perm_policy=detach, never
// silently downgraded to a plain move.
func TestFsMoveDetachRequiresManage(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkStorage(t, "/mv403", root)
	u := permUser(t, "mv403", true, false)

	code, body := callHandler(t, FsMove, u, "/api/fs/move",
		`{"src_dir":"/mv403/src","dst_dir":"/mv403/dst","names":["src"],"perm_policy":"detach"}`)
	if code != 403 {
		t.Fatalf("detach without manage must be 403, got %d: %s", code, body)
	}
}

// TestFsMoveDetachCrossStorage400: detach across a storage boundary is an
// explicit 400 (no cross-storage follow in stage 0), even for a caller that
// holds manage.
func TestFsMoveDetachCrossStorage400(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootA, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkStorage(t, "/mvx-a", rootA)
	mkStorage(t, "/mvx-b", rootB)
	u := permUser(t, "mvx", true, true)

	code, body := callHandler(t, FsMove, u, "/api/fs/move",
		`{"src_dir":"/mvx-a","dst_dir":"/mvx-b","names":["src"],"perm_policy":"detach"}`)
	if code != 400 {
		t.Fatalf("cross-storage move + detach must be 400, got %d: %s", code, body)
	}
}

// TestFsMoveInvalidPermPolicy400: an unrecognized perm_policy value is a 400.
func TestFsMoveInvalidPermPolicy400(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkStorage(t, "/mvbad", root)
	u := permUser(t, "mvbad", true, true)

	code, body := callHandler(t, FsMove, u, "/api/fs/move",
		`{"src_dir":"/mvbad/src","dst_dir":"/mvbad/dst","names":["src"],"perm_policy":"bogus"}`)
	if code != 400 {
		t.Fatalf("invalid perm_policy must be 400, got %d: %s", code, body)
	}
}

// TestFsCopyCopyPermsRequiresManage: copy_perms=true without manage is 403.
func TestFsCopyCopyPermsRequiresManage(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkStorage(t, "/cp403", root)
	u := permUser(t, "cp403", true, false)

	code, body := callHandler(t, FsCopy, u, "/api/fs/copy",
		`{"src_dir":"/cp403/src","dst_dir":"/cp403/dst","names":["src"],"copy_perms":true}`)
	if code != 403 {
		t.Fatalf("copy_perms without manage must be 403, got %d: %s", code, body)
	}
}

// TestFsCopyDefaultDoesNotClone is the regression guard: a default copy
// (copy_perms omitted) needs no manage right, succeeds synchronously on one
// storage, and clones NO permissions — behavior identical to before 0-C.
func TestFsCopyDefaultDoesNotClone(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := mkStorage(t, "/cpdef", root)
	// No manage right — a default copy must not need it.
	u := permUser(t, "cpdef", true, false)

	src := &model.Permission{Name: "cpdef-p", Path: "/cpdef/src", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true}
	if err := db.CreatePermission(src); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(src.ID) })

	code, body := callHandler(t, FsCopy, u, "/api/fs/copy",
		`{"src_dir":"/cpdef","dst_dir":"/cpdef/dst","names":["src"]}`)
	if code != 200 {
		t.Fatalf("default copy must succeed, got %d: %s", code, body)
	}
	// No clone at the destination anchor: a default copy never anchors a node
	// there, so nothing resolves.
	if _, found, err := db.GetNodeByChain(sid, "/dst/src"); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("default copy must clone no permissions, but a node was anchored at /dst/src")
	}
}

// TestFsCopyCopyPermsClones is the copy_perms=true acceptance path through the
// handler: a same-storage copy runs synchronously, so on return the source
// subtree's manual rows exist at the destination, bound to the same role, and a
// member of that role has the same effective access at the new location.
func TestFsCopyCopyPermsClones(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src/child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := mkStorage(t, "/cpok", root)

	deny := &model.Permission{Name: "cpok-deny", Path: "/cpok/src/child", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true}
	// The caller both drives the copy AND is the role member whose access we
	// verify at the destination; it needs fs rights + manage + the deny row.
	u := permUser(t, "cpok", true, true, deny)

	code, body := callHandler(t, FsCopy, u, "/api/fs/copy",
		`{"src_dir":"/cpok","dst_dir":"/cpok/dst","names":["src"],"copy_perms":true}`)
	if code != 200 {
		t.Fatalf("copy_perms copy must succeed, got %d: %s", code, body)
	}
	op.InvalidateRolePolicyForTest(uint(u.Role[0]))

	nodeID, found, err := db.GetNodeByChain(sid, "/dst/src/child")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("no node anchored at cloned /dst/src/child")
	}
	rows, err := db.GetManualPermissionsByNodeIDs([]uint{nodeID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected one cloned deny row at /dst/src/child, got %d", len(rows))
	}
	rids, err := db.GetRoleIDsByPermission(rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rids) != 1 || rids[0] != uint(u.Role[0]) {
		t.Fatalf("cloned row must be bound to the source role, got %v", rids)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(rows[0].ID) })
	if d, err := op.ResolveEffectivePolicy(u, "/cpok/dst/src/child"); err != nil || !d.Denied(model.ActRead) {
		t.Fatalf("role member must inherit the cloned deny at the destination: %+v %v", d, err)
	}
}

// TestFsRenameDetachDropsPermissions is the detach acceptance path through a
// synchronous handler (rename is in-place): after the rename the manual rows are
// gone and the renamed path falls back — deny row included.
func TestFsRenameDetachDropsPermissions(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "team"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkStorage(t, "/rndet", root)

	deny := &model.Permission{Name: "rndet-deny", Path: "/rndet/team", Origin: model.OriginManual,
		Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true}
	u := permUser(t, "rndet", true, true, deny)

	// Precondition: deny is live at the old path.
	if d, err := op.ResolveEffectivePolicy(u, "/rndet/team"); err != nil || !d.Denied(model.ActRead) {
		t.Fatalf("precondition: deny must be live before rename: %+v %v", d, err)
	}

	code, body := callHandler(t, FsRename, u, "/api/fs/rename",
		`{"path":"/rndet/team","name":"crew","perm_policy":"detach"}`)
	if code != 200 {
		t.Fatalf("rename detach must succeed, got %d: %s", code, body)
	}
	op.InvalidateRolePolicyForTest(uint(u.Role[0]))

	if _, err := db.GetPermissionById(deny.ID); err == nil {
		t.Fatal("deny row must be deleted by rename detach")
	}
	// The renamed path is no longer covered by the (now deleted) deny row.
	if d, err := op.ResolveEffectivePolicy(u, "/rndet/crew"); err != nil || d.Denied(model.ActRead) {
		t.Fatalf("renamed path must fall back after detach (no deny): %+v %v", d, err)
	}
}
