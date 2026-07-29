package handles

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
	"github.com/xhofe/tache"
)

// waitForMoveTasksDone polls fs.MoveTaskManager until every task it holds has
// reached a terminal state, instead of calling tache.Manager.Wait().
// tache.Manager.Wait() (github.com/xhofe/tache@v0.1.5, manager.go) reads its
// internal WorkerPool.working field directly, with no lock -- a genuine data
// race against WorkerPool.Get/Put, which do hold workersMutex. That race is
// entirely inside the third-party library (both the field and the offending
// read live there), so it can't be fixed from this side without forking
// tache; this sidesteps it by only using GetByCondition/GetState, which are
// already safe (GetByCondition ranges a concurrent map; GetState is
// synchronized via task.TaskExtension's own lock, see internal/task/base.go).
func waitForMoveTasksDone(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		done := true
		for _, mt := range fs.MoveTaskManager.GetAll() {
			switch mt.GetState() {
			case tache.StateSucceeded, tache.StateFailed, tache.StateCanceled, tache.StateErrored:
			default:
				done = false
			}
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for move tasks to finish")
		}
		time.Sleep(time.Millisecond)
	}
}

// FsMove always dispatches through fs.MoveTaskManager now (move is an async
// task, mirroring copy/upload); this package's tests never go through
// bootstrap.InitTaskManager, so without this the manager is nil and any
// FsMove call that reaches MoveAsTask panics.
func init() {
	fs.MoveTaskManager = tache.NewManager[*fs.MoveTask]()
}

// TestAuthzMatrix is the Step 5 closing regression net: one fixture (a role
// allow scope, a manual recursive deny nested under it, and a hide meta) is
// driven through the single-argument (path-addressed) handler entry points —
// FsList, FsGet, FsMkdir, FsRename, FsRecursiveMove — to pin down that the
// same deny/hide is honored identically no matter which handler is asked.
// It does NOT cover the name-addressed batch handlers (FsMove, FsCopy,
// FsRemove, FsBatchRename, FsRegexRename): those authorize once on a dir and
// then loop over caller-supplied names, so the interesting case is a deny
// nested under an authorized dir — see TestAuthzMatrixDenyFirstBypass.
// FsUp (middlewares package) is intentionally skipped here: it has its own
// coverage from Step 5 T1 and lives in a different package/test binary.

// respCode extracts the semantic response code. alist handlers funnel
// errors through common.ErrorResp/ErrorStrResp, which always write HTTP 200
// and carry the real code in the body's `{"code":...}` envelope; success
// responses use common.SuccessResp, code 200. Fall back to the raw HTTP
// status only when the body isn't that envelope.
func respCode(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	var envelope struct {
		Code *int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err == nil && envelope.Code != nil {
		return *envelope.Code
	}
	return w.Code
}

// mustScopePermIDs fetches the permission rows already bound to the role by
// CreateRole's scope sync, so a subsequent SetRolePermissions (replace
// semantics) can carry them forward alongside a manually added row instead
// of wiping them out.
func mustScopePermIDs(t *testing.T, roleID uint) []uint {
	t.Helper()
	ids, err := db.GetPermissionIDsByRole(roleID)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func mustMatrixMeta(t *testing.T, m *model.Meta) {
	t.Helper()
	if err := op.CreateMeta(m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteMetaById(m.ID) })
}

func setupMatrix(t *testing.T) (*model.User, string) {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"open", "den", "den2"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "open", "hid_x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// den2 carries a move-only deny (see below): it needs an actual file so
	// a recursive-move scenario has something concrete to try to move.
	if err := os.WriteFile(filepath.Join(dir, "den2", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sid, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "Local", MountPath: "/mx", Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Storage mount paths are globally unique in the DB (storages.mount_path);
	// without this cleanup, a second setupMatrix call in the same test binary
	// (each Fs* deny-first-bypass scenario runs its own) fails on a unique
	// constraint violation instead of getting a fresh fixture.
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })
	// role: allow /mx write+rename+move+copy+remove (bits 3,4,5,6,7); ActRead
	// is added implicitly by scopePermissionRows for every scope-covered
	// path.
	r := &model.Role{Name: "mx-role", PermissionScopes: []model.PermissionEntry{
		{Path: "/mx", Permission: 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7},
	}}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })
	// manual deny on /mx/den, all fs actions: the matrix's point is that the
	// same deny is honored identically by every entry point below.
	deny := &model.Permission{Name: "mx-deny", Path: "/mx/den", Recursive: true, Effect: model.EffectDeny,
		Actions: model.Actions{model.ActRead, model.ActWrite, model.ActRename, model.ActMove, model.ActCopy, model.ActRemove}}
	if err := op.CreatePermission(deny); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(deny.ID) })
	// manual deny on /mx/den2, ActMove only (read stays allowed): distinct
	// from the /mx/den deny above, which also denies ActRead and so gets
	// caught by FilterListing's read-based filtering before a batch handler
	// ever sees it. A move-only deny stays visible in listings, so it can
	// only be caught by a per-target Authorize call at the point of acting.
	moveOnlyDeny := &model.Permission{Name: "mx-den2-deny-move", Path: "/mx/den2", Recursive: true, Effect: model.EffectDeny,
		Actions: model.Actions{model.ActMove}}
	if err := op.CreatePermission(moveOnlyDeny); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(moveOnlyDeny.ID) })
	if err := op.SetRolePermissions(r.ID, append(mustScopePermIDs(t, r.ID), deny.ID, moveOnlyDeny.ID)); err != nil {
		t.Fatal(err)
	}
	mustMatrixMeta(t, &model.Meta{Path: "/mx/open", Hide: "hid.*"})
	return &model.User{ID: 2222, Username: "mxuser", Role: model.Roles{int(r.ID)}}, dir
}

func TestAuthzMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, _ := setupMatrix(t)
	cases := []struct {
		name    string
		run     func(c *gin.Context)
		method  string
		body    string
		want    int
		wantNot string // response body must not contain this substring
	}{
		{"list-open-hides", FsList, "POST", `{"path":"/mx/open"}`, 200, "hid_x.txt"},
		{"list-deny", FsList, "POST", `{"path":"/mx/den"}`, 403, ""},
		{"get-deny", FsGet, "POST", `{"path":"/mx/den/nope"}`, 403, ""},
		{"mkdir-open", FsMkdir, "POST", `{"path":"/mx/open/newdir"}`, 200, ""},
		{"mkdir-deny", FsMkdir, "POST", `{"path":"/mx/den/newdir"}`, 403, ""},
		{"rename-deny", FsRename, "POST", `{"path":"/mx/den/f","name":"g"}`, 403, ""},
		{"recursive-move-deny", FsRecursiveMove, "POST", `{"src_dir":"/mx/den","dst_dir":"/mx/open"}`, 403, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(tc.method, "/x", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(ctxkey.User, u)
			tc.run(c)
			if got := respCode(t, w); got != tc.want {
				t.Fatalf("want %d got %d body=%s", tc.want, got, w.Body.String())
			}
			if tc.wantNot != "" && strings.Contains(w.Body.String(), tc.wantNot) {
				t.Fatalf("response must not contain %q: %s", tc.wantNot, w.Body.String())
			}
		})
	}
}

// TestAuthzMatrixDenyFirstBypass is the deny-first bypass regression: the
// DIR-level Authorize (@/mx) these name-addressed batch handlers do up front
// passes, because the role allows remove/move/copy/rename on /mx broadly.
// Only a per-NAME Authorize (@/mx/den) consults the manual recursive deny
// nested under it. Without that per-name check, these handlers authorize
// once on the dir and then act directly on the denied subtree — a "denied"
// path gets deleted/moved/renamed anyway, just with a misleading 200.
//
// Each scenario gets its own setupMatrix fixture (rather than sharing
// TestAuthzMatrix's) because a correctly-blocked case is still expected to
// leave state untouched, but an unfixed/regressed case would actually
// mutate or delete "den" — sharing a fixture would let one broken subtest
// corrupt the next.
func TestAuthzMatrixDenyFirstBypass(t *testing.T) {
	gin.SetMode(gin.TestMode)

	run := func(t *testing.T, u *model.User, method, body string, handler func(c *gin.Context)) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, "/x", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(ctxkey.User, u)
		handler(c)
		return w
	}

	t.Run("remove", func(t *testing.T) {
		u, dir := setupMatrix(t)
		w := run(t, u, "POST", `{"dir":"/mx","names":["den"]}`, FsRemove)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("want 403 got %d body=%s", got, w.Body.String())
		}
		// A blocked remove must not have deleted the denied subtree either:
		// a deny that still lets the data get destroyed, just with the
		// wrong status code, is not a fix.
		if _, err := os.Stat(filepath.Join(dir, "den")); err != nil {
			t.Fatalf("denied dir must survive a bypassed FsRemove: stat: %v", err)
		}
	})

	t.Run("move", func(t *testing.T) {
		u, _ := setupMatrix(t)
		w := run(t, u, "POST", `{"src_dir":"/mx","dst_dir":"/mx/open","names":["den"]}`, FsMove)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("want 403 got %d body=%s", got, w.Body.String())
		}
	})

	t.Run("copy", func(t *testing.T) {
		u, _ := setupMatrix(t)
		w := run(t, u, "POST", `{"src_dir":"/mx","dst_dir":"/mx/open","names":["den"]}`, FsCopy)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("want 403 got %d body=%s", got, w.Body.String())
		}
	})

	t.Run("batch-rename", func(t *testing.T) {
		u, _ := setupMatrix(t)
		w := run(t, u, "POST", `{"src_dir":"/mx","rename_objects":[{"src_name":"den","new_name":"den3"}]}`, FsBatchRename)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("want 403 got %d body=%s", got, w.Body.String())
		}
	})

	// FsRecursiveMove builds its move set by walking fs.List +
	// op.FilterListing, which only excludes children with no ActRead — a
	// deny scoped to ActMove alone (read still allowed, like /mx/den2's
	// deny) stays visible in the listing and would be moved out from under
	// the deny without a per-file Authorize check at the point of the
	// actual move.
	t.Run("recursive-move-partial-deny", func(t *testing.T) {
		u, dir := setupMatrix(t)
		w := run(t, u, "POST", `{"src_dir":"/mx","dst_dir":"/mx/open"}`, FsRecursiveMove)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("want 403 got %d body=%s", got, w.Body.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "den2", "f.txt")); err != nil {
			t.Fatalf("move-denied file must survive a bypassed FsRecursiveMove: stat: %v", err)
		}
	})
}

// TestAuthzMatrixDstWriteAuthz pins down that move/copy authorize the
// destination directory for ActWrite, not just the source directory for
// ActMove/ActCopy. Before the fix, a user with rights only on the source
// side could still move or copy content INTO /mx/den — dstDir was never
// consulted, so a "denied" directory received writes anyway. Each scenario
// gets its own setupMatrix fixture because the allow-case actually performs
// the move, which would corrupt state for a case sharing a fixture.
func TestAuthzMatrixDstWriteAuthz(t *testing.T) {
	gin.SetMode(gin.TestMode)

	run := func(t *testing.T, u *model.User, method, body string, handler func(c *gin.Context)) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, "/x", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(ctxkey.User, u)
		handler(c)
		return w
	}

	t.Run("move-dst-deny", func(t *testing.T) {
		u, dir := setupMatrix(t)
		w := run(t, u, "POST", `{"src_dir":"/mx/open","dst_dir":"/mx/den","names":["hid_x.txt"]}`, FsMove)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("want 403 got %d body=%s", got, w.Body.String())
		}
		// A blocked move must not have relocated the file either: a deny
		// that still lets the write land, just with the wrong status code,
		// is not a fix.
		if _, err := os.Stat(filepath.Join(dir, "open", "hid_x.txt")); err != nil {
			t.Fatalf("source file must survive a dst-denied FsMove: stat: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "den", "hid_x.txt")); err == nil {
			t.Fatalf("file must not land in a dst-denied directory")
		}
	})

	t.Run("copy-dst-deny", func(t *testing.T) {
		u, dir := setupMatrix(t)
		w := run(t, u, "POST", `{"src_dir":"/mx/open","dst_dir":"/mx/den","names":["hid_x.txt"]}`, FsCopy)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("want 403 got %d body=%s", got, w.Body.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "den", "hid_x.txt")); err == nil {
			t.Fatalf("file must not be copied into a dst-denied directory")
		}
	})

	// Control: a dst that's allowed (plain /mx, no deny nested there) must
	// not be rejected for the dst reason — proves the new check isn't
	// blanket-denying every move/copy.
	t.Run("move-dst-allow", func(t *testing.T) {
		u, dir := setupMatrix(t)
		w := run(t, u, "POST", `{"src_dir":"/mx/open","dst_dir":"/mx","names":["hid_x.txt"]}`, FsMove)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("want 200 got %d body=%s", got, w.Body.String())
		}
		// Move is now dispatched as a background task (parity with copy/
		// upload): the handler returns as soon as the task is queued, so the
		// relocation must be awaited before checking the filesystem.
		waitForMoveTasksDone(t)
		if _, err := os.Stat(filepath.Join(dir, "hid_x.txt")); err != nil {
			t.Fatalf("dst-allowed FsMove should have relocated the file: stat: %v", err)
		}
	})

	t.Run("recursive-move-dst-deny", func(t *testing.T) {
		u, dir := setupMatrix(t)
		w := run(t, u, "POST", `{"src_dir":"/mx/open","dst_dir":"/mx/den"}`, FsRecursiveMove)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("want 403 got %d body=%s", got, w.Body.String())
		}
		// A blocked recursive move must not have relocated any files either:
		// a deny on the destination that still lets the write land, just with
		// the wrong status code, is not a fix.
		if _, err := os.Stat(filepath.Join(dir, "open", "hid_x.txt")); err != nil {
			t.Fatalf("source file must survive a dst-denied FsRecursiveMove: stat: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "den", "hid_x.txt")); err == nil {
			t.Fatalf("file must not land in a dst-denied directory")
		}
	})
}
