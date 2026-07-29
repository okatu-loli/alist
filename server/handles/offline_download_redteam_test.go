package handles

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/offline_download"
	"github.com/gin-gonic/gin"
)

// driveTid runs a targeted task handler (?tid=) with the caller in context.
func driveTid(u *model.User, tid string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/x?tid="+tid, nil)
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

// TestOfflineDownloadTaskCreatorIsolation is the cross-creator isolation
// regression for the /offline_download task route group. The task API is wired
// through the same generic taskRoute as upload/copy/move, whose
// getTargetedHandler gates every lookup on `isAdmin || uid == creator.ID` and
// returns 404 (not 403) to a non-owner so task ids can't be probed. This test
// proves that gate actually protects offline-download tasks: user A queues a
// task; a different non-admin user B can neither read nor delete it (404, and it
// survives), while the owner A and an admin can read it.
//
// Deleting the `!isAdmin && uid != t.GetCreator().ID` check in getTargetedHandler
// turns this red.
func TestOfflineDownloadTaskCreatorIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)
	mgr := offline_download.OfflineDownloadTaskManager

	// User A queues an offline-download task through the real handler.
	mount := setupOfflineFixture(t)
	userA := offlineUser(t, mount, bitWrite|bitOfflineDownload)
	body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"]}`, mount+"/dst")
	w := driveJSON(userA, "POST", body, AddOfflineDownload)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("owner submit must be 200, got %d: %s", got, w.Body.String())
	}
	tid := parseTaskInfos(t, w)[0].ID

	// A different non-admin user (not the creator) must not see it.
	userB := nonAdminUser(t)
	if got := respCode(t, driveTid(userB, tid, taskInfoHandler(mgr))); got != 404 {
		t.Fatalf("non-owner info must be 404 (isolation), got %d", got)
	}
	// ...and must not be able to delete it.
	if got := respCode(t, driveTid(userB, tid, taskDeleteHandler(mgr))); got != 404 {
		t.Fatalf("non-owner delete must be 404 (isolation), got %d", got)
	}
	if _, ok := mgr.GetByID(tid); !ok {
		t.Fatal("a non-owner's delete must NOT remove the task")
	}

	// The owner can read it.
	if got := respCode(t, driveTid(userA, tid, taskInfoHandler(mgr))); got != 200 {
		t.Fatalf("owner info must be 200, got %d", got)
	}
	// An admin can read it.
	admin := &model.User{ID: 51234, Username: "od-admin", Role: model.Roles{model.ADMIN}}
	if got := respCode(t, driveTid(admin, tid, taskInfoHandler(mgr))); got != 200 {
		t.Fatalf("admin info must be 200, got %d", got)
	}
}

// TestAddOfflineDownloadPathTraversalContained pins that a malformed target path
// laced with traversal cannot escape the caller's scoped subtree: user.JoinPath
// folds it and op.Authorize denies anything outside the granted /dst subtree.
// The user has both bits, but only on <mount>/dst; a path that resolves above
// /dst must be a 403 with no task queued.
func TestAddOfflineDownloadPathTraversalContained(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)
	mgr := offline_download.OfflineDownloadTaskManager

	mount := setupOfflineFixture(t)
	u := offlineUser(t, mount, bitWrite|bitOfflineDownload) // rights only on <mount>/dst

	// Each of these, once JoinPath-folded, escapes /dst (the only authorized
	// subtree) and must be denied.
	for _, p := range []string{
		mount + "/dst/../../etc",
		mount + "/dst/../sibling",
		mount, // the mount root itself, above /dst
	} {
		before := len(mgr.GetAll())
		body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"]}`, p)
		w := driveJSON(u, "POST", body, AddOfflineDownload)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("path %q escaping the scoped subtree must be 403, got %d: %s", p, got, w.Body.String())
		}
		if after := len(mgr.GetAll()); after != before {
			t.Fatalf("denied path %q must queue no task: %d -> %d", p, before, after)
		}
	}
}
