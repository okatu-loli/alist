package offline_download

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/xhofe/tache"
)

// newTestServer returns an httptest server that serves body with a
// Content-Disposition naming it fileName, for exercising the SimpleHttp path.
func newTestServer(t *testing.T, fileName, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="`+fileName+`"`)
		io.WriteString(w, body)
	}))
}

// newPausedTransferManager installs a fresh, PAUSED TransferTaskManager into the
// package var and restores the previous value on cleanup. Paused (WithRunning
// false) means transferStd's Add() queues tasks without dispatching them to a
// worker goroutine, so a test can retrieve them with GetAll() and run each one
// synchronously on its own goroutine — deterministic and free of the
// tache-internal SetState/GetState race that polling a manager-run task trips
// under -race (same rationale as runTask in task_test.go).
func newPausedTransferManager(t *testing.T) *tache.Manager[*TransferTask] {
	t.Helper()
	prev := TransferTaskManager
	TransferTaskManager = tache.NewManager[*TransferTask](tache.WithRunning(false))
	t.Cleanup(func() { TransferTaskManager = prev })
	return TransferTaskManager
}

// drainTransferTasks runs every queued TransferTask synchronously and invokes
// the DeletePolicy hook (OnSucceeded/OnFailed) tache would otherwise call, so the
// test observes the same end state (dest files written, temp sources deleted per
// policy) a live manager would produce — without any concurrency.
func drainTransferTasks(t *testing.T, mgr *tache.Manager[*TransferTask]) {
	t.Helper()
	for _, task := range mgr.GetAll() {
		task.SetCreator(&model.User{ID: 1, Username: "downloader"})
		task.SetCtx(context.Background())
		if err := task.Run(); err != nil {
			task.OnFailed()
			t.Fatalf("TransferTask.Run() = %v; want success", err)
		}
		task.OnSucceeded()
	}
}

// TestTransferTaskRun drives a single TransferTask end to end: a local temp file
// is streamed into the Local destination storage via op.Put, progress and total
// bytes are recorded, and DeleteOnUploadSucceed removes the temp source.
func TestTransferTaskRun(t *testing.T) {
	dstRoot := setupLocalStorage(t, "/xfer-one")

	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "movie.bin")
	want := make([]byte, 300)
	for i := range want {
		want[i] = byte('A' + i%26)
	}
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatalf("write temp src: %v", err)
	}

	task := &TransferTask{
		SrcFilePath:  src,
		DstStorageMp: "/xfer-one",
		DstDirPath:   "/",
		DeletePolicy: DeleteOnUploadSucceed,
		FileSize:     int64(len(want)),
	}
	task.SetCreator(&model.User{ID: 1, Username: "downloader"})
	task.SetCtx(context.Background())
	if err := task.Run(); err != nil {
		t.Fatalf("Run() = %v; want success", err)
	}
	task.OnSucceeded()

	got, err := os.ReadFile(filepath.Join(dstRoot, "movie.bin"))
	if err != nil || string(got) != string(want) {
		t.Fatalf("dest content = %q err=%v, want the source bytes", got, err)
	}
	if task.GetTotalBytes() != int64(len(want)) {
		t.Errorf("total bytes = %d, want %d", task.GetTotalBytes(), len(want))
	}
	if task.GetProgress() < 100 {
		t.Errorf("progress = %v, want it to reach 100", task.GetProgress())
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("DeleteOnUploadSucceed must remove the temp source; stat err = %v", err)
	}
}

// TestTransferStdEnumeratesTree pins transferStd's enumeration + subdir
// preservation: a temp tree with a top-level file and a nested-subdir file
// enqueues one TransferTask each, and running them lands both in the destination
// storage with the sub-directory structure preserved. DeleteOnUploadSucceed then
// removes the temp sources.
func TestTransferStdEnumeratesTree(t *testing.T) {
	mgr := newPausedTransferManager(t)
	dstRoot := setupLocalStorage(t, "/xfer-tree")

	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "top.bin"), []byte("top-data"), 0o644); err != nil {
		t.Fatalf("write top file: %v", err)
	}
	sub := filepath.Join(tempDir, "season1")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "ep1.bin"), []byte("ep1-data"), 0o644); err != nil {
		t.Fatalf("write sub file: %v", err)
	}

	ctx := context.WithValue(context.Background(), ctxkey.User, &model.User{ID: 1, Username: "downloader"})
	if err := transferStd(ctx, tempDir, "/xfer-tree", "/", DeleteOnUploadSucceed); err != nil {
		t.Fatalf("transferStd() = %v", err)
	}

	if n := len(mgr.GetAll()); n != 2 {
		t.Fatalf("transferStd enqueued %d tasks, want 2 (one per regular file)", n)
	}
	drainTransferTasks(t, mgr)

	// Top-level file lands directly under the dest root.
	if b, err := os.ReadFile(filepath.Join(dstRoot, "top.bin")); err != nil || string(b) != "top-data" {
		t.Fatalf("dest top.bin = %q err=%v", b, err)
	}
	// Nested file preserves its sub-directory position.
	if b, err := os.ReadFile(filepath.Join(dstRoot, "season1", "ep1.bin")); err != nil || string(b) != "ep1-data" {
		t.Fatalf("dest season1/ep1.bin = %q err=%v", b, err)
	}
	// Temp sources removed by DeleteOnUploadSucceed.
	if _, err := os.Stat(filepath.Join(tempDir, "top.bin")); !os.IsNotExist(err) {
		t.Errorf("temp top.bin should be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "ep1.bin")); !os.IsNotExist(err) {
		t.Errorf("temp season1/ep1.bin should be deleted, stat err = %v", err)
	}
}

// TestTransferStdDeleteNeverKeepsSource proves the DeletePolicy knob: with
// DeleteNever the transfer still writes the destination file but leaves the temp
// source in place.
func TestTransferStdDeleteNeverKeepsSource(t *testing.T) {
	mgr := newPausedTransferManager(t)
	dstRoot := setupLocalStorage(t, "/xfer-keep")

	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "keep.bin")
	if err := os.WriteFile(src, []byte("keep-me"), 0o644); err != nil {
		t.Fatalf("write temp src: %v", err)
	}

	ctx := context.Background()
	if err := transferStd(ctx, tempDir, "/xfer-keep", "/", DeleteNever); err != nil {
		t.Fatalf("transferStd() = %v", err)
	}
	drainTransferTasks(t, mgr)

	if b, err := os.ReadFile(filepath.Join(dstRoot, "keep.bin")); err != nil || string(b) != "keep-me" {
		t.Fatalf("dest keep.bin = %q err=%v", b, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("DeleteNever must keep the temp source; stat err = %v", err)
	}
}

// fakeDaemonTool is an ext.OfflineDownloadTool double for the DownloadTask daemon
// branch. AddURL writes a finished file into args.TempDir (as a real daemon would
// after downloading); Status reports Completed only after completeAfter polls, so
// the poll loop is actually exercised. removeCalls records Remove() invocations.
type fakeDaemonTool struct {
	name          string
	fileName      string
	fileData      []byte
	completeAfter int

	polls       int
	removeCalls int
	addedGID    string
}

func (f *fakeDaemonTool) Manifest() ext.Manifest                     { return ext.Manifest{ID: f.name} }
func (f *fakeDaemonTool) Init(ctx context.Context, h ext.Host) error { return nil }
func (f *fakeDaemonTool) Shutdown(ctx context.Context) error         { return nil }
func (f *fakeDaemonTool) Config() ext.OfflineDownloadToolConfig {
	return ext.OfflineDownloadToolConfig{Name: f.name}
}

func (f *fakeDaemonTool) AddURL(ctx context.Context, args ext.AddArgs) (string, error) {
	if err := os.MkdirAll(args.TempDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(args.TempDir, f.fileName), f.fileData, 0o644); err != nil {
		return "", err
	}
	f.addedGID = "gid-" + f.name
	return f.addedGID, nil
}

func (f *fakeDaemonTool) Remove(ctx context.Context, gid string) error {
	f.removeCalls++
	return nil
}

func (f *fakeDaemonTool) Status(ctx context.Context, gid string) (ext.ToolStatus, error) {
	f.polls++
	if f.polls >= f.completeAfter {
		return ext.ToolStatus{TotalBytes: int64(len(f.fileData)), Progress: 100, Completed: true}, nil
	}
	return ext.ToolStatus{TotalBytes: int64(len(f.fileData)), Progress: 50}, nil
}

// fastPoll shrinks the daemon poll cadence for the duration of a test so the
// poll loop doesn't wait real seconds between Status() calls.
func fastPoll(t *testing.T) {
	t.Helper()
	prev := daemonPollInterval
	daemonPollInterval = 2 * time.Millisecond
	t.Cleanup(func() { daemonPollInterval = prev })
}

// TestDownloadTaskDaemonBranch drives the full daemon lifecycle: a Toolname-set
// DownloadTask hands the URL to the fake tool (which writes a temp file), polls
// Status until Completed, transfers the temp file into the Local destination
// storage, and removes the daemon-side task.
func TestDownloadTaskDaemonBranch(t *testing.T) {
	fastPoll(t)
	mgr := newPausedTransferManager(t)
	resetBase(t)

	fake := regTool(t, "daemontool")
	daemon := &fakeDaemonTool{name: "daemontool", fileName: "result.bin", fileData: []byte("daemon-downloaded-bytes"), completeAfter: 2}
	// Re-register with the fully-behaved daemon double (regTool installed a bare
	// baseFakeTool; RegisterTool swaps the trusted instance in place).
	RegisterTool("daemontool", "builtin.offline_download.daemontool", daemon)
	_ = fake
	if err := Init("daemontool"); err != nil {
		t.Fatalf("Init(daemontool): %v", err)
	}
	t.Cleanup(func() { resetBase(t) })

	dstRoot := setupLocalStorage(t, "/od-daemon")

	tempDir := filepath.Join(t.TempDir(), "daemontool", "abc123")
	task := &DownloadTask{
		URL:              "magnet:?xt=urn:btih:deadbeef",
		DstStorageMp:     "/od-daemon",
		DstDirActualPath: "/",
		Toolname:         "daemontool",
		TempDir:          tempDir,
	}
	if err := runTask(t, task); err != nil {
		t.Fatalf("daemon Run() = %v; want success", err)
	}

	// The daemon path enqueued the transfer; drain it to move the file.
	drainTransferTasks(t, mgr)

	if b, err := os.ReadFile(filepath.Join(dstRoot, "result.bin")); err != nil || string(b) != "daemon-downloaded-bytes" {
		t.Fatalf("dest result.bin = %q err=%v, want the daemon's bytes", b, err)
	}
	if daemon.removeCalls != 1 {
		t.Errorf("tool.Remove called %d times, want exactly 1 after transfer", daemon.removeCalls)
	}
	if daemon.polls < daemon.completeAfter {
		t.Errorf("Status polled %d times, want >= %d", daemon.polls, daemon.completeAfter)
	}
	if task.GetProgress() < 100 {
		t.Errorf("daemon task progress = %v, want it to reach 100", task.GetProgress())
	}
	if task.GID != "gid-daemontool" {
		t.Errorf("task.GID = %q, want gid-daemontool", task.GID)
	}
}

// TestDownloadTaskDaemonFallback proves the friendly fallback: a Toolname-set
// task whose tool is NOT active at run time (never registered) falls back to the
// SimpleHttp direct path rather than failing. A Local dest (no PutURL) + httptest
// source lands the file via the SimpleHttp stream.
func TestDownloadTaskDaemonFallback(t *testing.T) {
	resetBase(t)
	// No tool registered/selected for "ghosttool": ActiveTool() == nil.

	prev := allowLoopbackForTest
	allowLoopbackForTest = true
	t.Cleanup(func() { allowLoopbackForTest = prev })

	want := "fallback-body-contents-well-over-a-hundred-bytes-so-progress-fires-" +
		"0123456789012345678901234567890123456789012345678901234567890123456789"
	srv := newTestServer(t, "fallback.bin", want)
	defer srv.Close()

	dstRoot := setupLocalStorage(t, "/od-fallback")

	task := &DownloadTask{
		URL:              srv.URL,
		DstStorageMp:     "/od-fallback",
		DstDirActualPath: "/",
		Toolname:         "ghosttool", // set, but no active tool -> SimpleHttp fallback
		TempDir:          filepath.Join(t.TempDir(), "ghost"),
	}
	if err := runTask(t, task); err != nil {
		t.Fatalf("fallback Run() = %v; want success via SimpleHttp", err)
	}
	if task.FileName != "fallback.bin" {
		t.Errorf("derived FileName = %q, want fallback.bin (SimpleHttp path ran)", task.FileName)
	}
	got, err := os.ReadFile(filepath.Join(dstRoot, "fallback.bin"))
	if err != nil || string(got) != want {
		t.Fatalf("dest fallback.bin = %q err=%v, want %q", got, err, want)
	}
}
