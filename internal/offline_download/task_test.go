package offline_download

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	// Import the concrete drivers this test exercises directly (not the
	// "drivers" aggregator) to register them without pulling the whole set:
	// Local backs the SimpleHttp put path, UrlTree backs the PutURL path.
	_ "github.com/alist-org/alist/v3/drivers/local"
	_ "github.com/alist-org/alist/v3/drivers/url_tree"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/xhofe/tache"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:offline_download_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
}

func setupLocalStorage(t *testing.T, mountPath string) string {
	t.Helper()
	root := t.TempDir()
	if _, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mountPath,
		Driver:    "Local",
		Addition:  `{"root_folder_path":"` + root + `"}`,
	}); err != nil {
		t.Fatalf("create local storage %s: %v", mountPath, err)
	}
	t.Cleanup(func() {
		if s, err := op.GetStorageByMountPath(mountPath); err == nil {
			_ = op.DeleteStorageById(context.Background(), s.GetStorage().ID)
		}
	})
	return root
}

// setupUrlTreeStorage registers a writable, index-only UrlTree storage. UrlTree
// implements driver.PutURLResult but never fetches the URL (HeadSize is off), so
// it's the ideal probe for "PutURL preferred, SimpleHttp not touched".
func setupUrlTreeStorage(t *testing.T, mountPath string) {
	t.Helper()
	if _, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mountPath,
		Driver:    "UrlTree",
		// Empty structure -> an empty writable root; HeadSize off -> PutURL
		// records the URL without any HTTP HEAD.
		Addition: `{"url_structure":"","head_size":false,"writable":true}`,
	}); err != nil {
		t.Fatalf("create url_tree storage %s: %v", mountPath, err)
	}
	t.Cleanup(func() {
		if s, err := op.GetStorageByMountPath(mountPath); err == nil {
			_ = op.DeleteStorageById(context.Background(), s.GetStorage().ID)
		}
	})
}

// runTask executes task.Run() synchronously on the calling goroutine. Seeding
// the Base ctx up front (what tache.Manager would otherwise do on dispatch)
// lets Run() build its user-scoped ctx without a nil-parent panic, while
// keeping the whole run single-goroutine -- deterministic and free of the
// tache-internal SetState/GetState data race that polling a manager-run task
// from the test goroutine trips under -race.
func runTask(t *testing.T, task *DownloadTask) error {
	t.Helper()
	task.SetCreator(&model.User{ID: 1, Username: "downloader"})
	task.SetCtx(context.Background())
	return task.Run()
}

// TestDownloadTaskRunSimpleHttp drives the fallback path end to end: a Local
// storage (no PutURL support) forces SimpleHttp, the httptest body lands at the
// destination with the Content-Disposition-derived name, and progress is
// recorded. Body is >=100 bytes so CopyWithCtx's size/100 progress step is
// non-zero and the progress callback actually fires.
func TestDownloadTaskRunSimpleHttp(t *testing.T) {
	prev := allowLoopbackForTest
	allowLoopbackForTest = true // httptest listens on 127.0.0.1
	t.Cleanup(func() { allowLoopbackForTest = prev })

	want := strings.Repeat("A", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="payload.bin"`)
		io.WriteString(w, want)
	}))
	defer srv.Close()

	dstRoot := setupLocalStorage(t, "/od-simplehttp")

	task := &DownloadTask{
		URL:              srv.URL,
		DstStorageMp:     "/od-simplehttp",
		DstDirActualPath: "/",
		// FileName intentionally empty: Run() must derive it from the response
		// and write it back.
	}
	if err := runTask(t, task); err != nil {
		t.Fatalf("Run() = %v; want success", err)
	}
	if task.FileName != "payload.bin" {
		t.Errorf("derived FileName = %q, want %q", task.FileName, "payload.bin")
	}
	got, err := os.ReadFile(filepath.Join(dstRoot, "payload.bin"))
	if err != nil || string(got) != want {
		t.Fatalf("downloaded content = %q err=%v, want %q", got, err, want)
	}
	if task.GetTotalBytes() != int64(len(want)) {
		t.Errorf("total bytes = %d, want %d", task.GetTotalBytes(), len(want))
	}
	if task.GetProgress() < 100 {
		t.Errorf("progress = %v, want it to reach 100", task.GetProgress())
	}
}

// TestDownloadTaskRunPreferPutURL proves dispatch order: with a driver that
// implements PutURL, Run() must NOT fall back to SimpleHttp. The URL points at
// an internal address (10.0.0.1) that Fetch would refuse outright, so a success
// can only mean the PutURL branch ran and no HTTP fetch was attempted.
func TestDownloadTaskRunPreferPutURL(t *testing.T) {
	// Guard: keep loopback disallowed so there is no accidental fetch escape.
	prev := allowLoopbackForTest
	allowLoopbackForTest = false
	t.Cleanup(func() { allowLoopbackForTest = prev })

	setupUrlTreeStorage(t, "/od-puturl")

	const unreachable = "http://10.0.0.1:1/never-fetched"
	task := &DownloadTask{
		URL:              unreachable,
		DstStorageMp:     "/od-puturl",
		DstDirActualPath: "/",
		FileName:         "indexed.bin",
	}
	if err := runTask(t, task); err != nil {
		t.Fatalf("Run() = %v; PutURL should have handled it without fetching", err)
	}

	// The URL must now be indexed in the UrlTree storage -- confirming PutURL
	// actually recorded it rather than the task no-opping.
	storage, err := op.GetStorageByMountPath("/od-puturl")
	if err != nil {
		t.Fatalf("resolve storage: %v", err)
	}
	obj, err := op.Get(context.Background(), storage, "/indexed.bin")
	if err != nil {
		t.Fatalf("expected indexed.bin recorded via PutURL, get err: %v", err)
	}
	if obj.GetName() != "indexed.bin" {
		t.Errorf("indexed obj name = %q, want indexed.bin", obj.GetName())
	}
}

// TestDownloadTask_JSONRoundTrip pins that every field Run() needs survives a
// persist/recover cycle (the task is re-runnable purely from these), while the
// embedded tache/task state round-trips too.
func TestDownloadTask_JSONRoundTrip(t *testing.T) {
	original := &DownloadTask{
		URL:              "https://example.com/big.iso",
		DstStorageMp:     "/dst",
		DstDirActualPath: "/dir",
		FileName:         "big.iso",
	}
	original.SetID("od-1")
	original.SetState(tache.StateRunning)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var restored DownloadTask
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.URL != original.URL ||
		restored.DstStorageMp != original.DstStorageMp ||
		restored.DstDirActualPath != original.DstDirActualPath ||
		restored.FileName != original.FileName {
		t.Fatalf("persistable fields did not round-trip: got %+v", &restored)
	}
	if restored.GetID() != "od-1" || restored.GetState() != tache.StateRunning {
		t.Errorf("tache.Base fields did not round-trip: id=%q state=%v", restored.GetID(), restored.GetState())
	}
	args := restored.GetNameArgs()
	if len(args) != 6 || args[1] != "https://example.com/big.iso" || args[3] != "/dst" || args[5] != "/dir" {
		t.Errorf("GetNameArgs() = %v", args)
	}
}
