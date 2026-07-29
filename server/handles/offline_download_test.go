package handles

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/offline_download"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
	"github.com/xhofe/tache"
)

// Offline-download handler authorization tests (plan Wave 3). The gate is the
// load-bearing part: submitting an offline download creates a file in the
// target directory, so it must require BOTH ActWrite (the root write boundary)
// and ActOfflineDownload (the capability bit) on the target path — either
// missing is a 403. These are driven end to end against a real Local storage +
// scoped role, mirroring labels_test.go.

// offlineTestManagerOnce installs a PAUSED offline-download task manager for the
// package's tests. Bootstrap never runs here, so the production
// OfflineDownloadTaskManager is nil; a paused manager (WithRunning(false)) lets
// the handler Add() tasks and lets the tests inspect them, while guaranteeing no
// task ever dispatches a real network download during the test run.
var offlineTestManagerOnce sync.Once

func ensureOfflineDownloadManager(t *testing.T) {
	t.Helper()
	offlineTestManagerOnce.Do(func() {
		offline_download.OfflineDownloadTaskManager =
			tache.NewManager[*offline_download.DownloadTask](
				tache.WithWorks(1),
				tache.WithRunning(false),
			)
	})
}

// setupOfflineFixture builds an isolated Local storage mounted at a unique
// /odN with an empty "dst" subtree, and returns the mount path. Callers attach
// whichever permission combination the case under test needs via a scoped role.
func setupOfflineFixture(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&labelFixtureSeq, 1)
	mount := fmt.Sprintf("/od%d", n)

	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/dst", 0o755); err != nil {
		t.Fatal(err)
	}
	sid, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })
	return mount
}

// offlineUser makes a non-admin user carrying exactly the given permission bits
// on <mount>/dst (bit 2 == ActOfflineDownload, bit 3 == ActWrite).
func offlineUser(t *testing.T, mount string, perm int32) *model.User {
	t.Helper()
	n := atomic.AddInt64(&labelFixtureSeq, 1)
	r := createScopedRole(t, fmt.Sprintf("od-role-%d", n), []model.PermissionEntry{
		{Path: mount + "/dst", Permission: perm},
	})
	return &model.User{ID: 70000 + uint(n), Username: fmt.Sprintf("oduser%d", n), Role: model.Roles{int(r.ID)}}
}

// parseTaskInfos unwraps a common.Resp[[]TaskInfo] response body.
func parseTaskInfos(t *testing.T, w *httptest.ResponseRecorder) []TaskInfo {
	t.Helper()
	var env struct {
		Data []TaskInfo `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse task infos: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

const (
	bitOfflineDownload int32 = 1 << 2 // model.ActOfflineDownload
	bitWrite           int32 = 1 << 3 // model.ActWrite
)

// TestAddOfflineDownloadAuthz pins the double gate: neither bit alone is enough;
// only a caller with BOTH ActWrite and ActOfflineDownload may queue a task, and
// on success the task actually lands in the manager with the submitted URL.
func TestAddOfflineDownloadAuthz(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)

	t.Run("no-write-403", func(t *testing.T) {
		mount := setupOfflineFixture(t)
		u := offlineUser(t, mount, bitOfflineDownload) // capability but no write
		body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"]}`, mount+"/dst")
		w := driveJSON(u, "POST", body, AddOfflineDownload)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("no ActWrite must be 403, got %d: %s", got, w.Body.String())
		}
	})

	t.Run("write-without-capability-403", func(t *testing.T) {
		mount := setupOfflineFixture(t)
		u := offlineUser(t, mount, bitWrite) // write but no offline-download capability
		body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/a.bin"]}`, mount+"/dst")
		w := driveJSON(u, "POST", body, AddOfflineDownload)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("ActWrite without ActOfflineDownload must be 403, got %d: %s", got, w.Body.String())
		}
	})

	t.Run("both-permissions-queues-task", func(t *testing.T) {
		mount := setupOfflineFixture(t)
		u := offlineUser(t, mount, bitWrite|bitOfflineDownload)
		theURL := "http://example.com/queued.bin"
		body := fmt.Sprintf(`{"path":%q,"urls":[%q]}`, mount+"/dst", theURL)
		w := driveJSON(u, "POST", body, AddOfflineDownload)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("both permissions must be 200, got %d: %s", got, w.Body.String())
		}
		infos := parseTaskInfos(t, w)
		if len(infos) != 1 {
			t.Fatalf("expected exactly one queued task, got %d: %s", len(infos), w.Body.String())
		}
		task, ok := offline_download.OfflineDownloadTaskManager.GetByID(infos[0].ID)
		if !ok {
			t.Fatalf("queued task %q not found in manager", infos[0].ID)
		}
		if task.URL != theURL {
			t.Fatalf("queued task URL = %q, want %q", task.URL, theURL)
		}
		if task.GetCreator() == nil || task.GetCreator().ID != u.ID {
			t.Fatalf("queued task creator must be the submitting user")
		}
	})
}

// TestAddOfflineDownloadRejectsBadScheme pins the all-or-nothing URL validation:
// a batch containing a non-http(s) URL is rejected wholesale (400) and NOT one
// valid-URL task is created from it — a partial submission would be worse than a
// clean, retryable failure.
func TestAddOfflineDownloadRejectsBadScheme(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)

	mount := setupOfflineFixture(t)
	u := offlineUser(t, mount, bitWrite|bitOfflineDownload)

	before := len(offline_download.OfflineDownloadTaskManager.GetAll())
	body := fmt.Sprintf(`{"path":%q,"urls":["http://example.com/ok.bin","file:///etc/passwd"]}`, mount+"/dst")
	w := driveJSON(u, "POST", body, AddOfflineDownload)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("a batch with a non-http(s) URL must be 400, got %d: %s", got, w.Body.String())
	}
	if after := len(offline_download.OfflineDownloadTaskManager.GetAll()); after != before {
		t.Fatalf("a rejected batch must create no task: %d -> %d", before, after)
	}
}

// TestAddOfflineDownloadRejectsEmpty pins that a submission with no URLs is a
// 400 rather than a silent no-op success.
func TestAddOfflineDownloadRejectsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ensureOfflineDownloadManager(t)

	mount := setupOfflineFixture(t)
	u := offlineUser(t, mount, bitWrite|bitOfflineDownload)
	body := fmt.Sprintf(`{"path":%q,"urls":[]}`, mount+"/dst")
	w := driveJSON(u, "POST", body, AddOfflineDownload)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("empty urls must be 400, got %d: %s", got, w.Body.String())
	}
}
