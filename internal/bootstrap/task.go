package bootstrap

import (
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/internal/task"
	"github.com/xhofe/tache"
)

func taskFilterNegative(num int) int64 {
	if num < 0 {
		num = 0
	}
	return int64(num)
}

func InitTaskManager() {
	// bootstrap.LoadStorages() (called before this) only *starts* loading
	// storages -- it hands the work off to a goroutine and returns
	// immediately, flipping conf.StoragesLoaded once every storage has
	// finished (see storage.go and server/middlewares.StoragesLoaded, which
	// gates the fs API on the same flag). A recovered Copy/Move/Upload task
	// re-resolves its storage by mount path the moment it runs
	// (op.GetStorageByMountPath), so building these managers running -- which
	// dispatches every recovered task to a worker synchronously, before
	// InitTaskManager even returns -- races that goroutine: whichever
	// storages haven't finished registering yet make every task that needs
	// them fail with "no mount path for a storage", burning through
	// MaxRetry and landing in StateFailed instead of resuming. So every
	// manager below starts paused (tache.WithRunning(false)): recovery still
	// loads and queues tasks immediately, but startTaskManagersOnStoragesLoaded
	// only lets them actually run once conf.StoragesLoaded is true.
	uploadOpts := []tache.Option{tache.WithWorks(setting.GetInt(conf.TaskUploadThreadsNum, conf.Conf.Tasks.Upload.Workers)), tache.WithPersistFunction(db.GetTaskDataFunc("upload", conf.Conf.Tasks.Upload.TaskPersistant), db.UpdateTaskDataFunc("upload", conf.Conf.Tasks.Upload.TaskPersistant)), tache.WithMaxRetry(conf.Conf.Tasks.Upload.MaxRetry), tache.WithRunning(false)}
	// task.AssertPersistDebounceAsync guards the invariant TaskExtension's
	// locked Set* methods depend on (see its doc comment in
	// internal/task/base.go's mu field): none of the options above pass
	// tache.WithOptions (the only Option that can null tache's default
	// PersistDebounce back out), but re-run tache's own option resolution
	// against this exact slice so a future change that did would panic here
	// at boot instead of deadlocking silently under load.
	task.AssertPersistDebounceAsync(uploadOpts...)
	fs.UploadTaskManager = tache.NewManager[*fs.UploadTask](uploadOpts...)
	op.RegisterSettingChangingCallback(func() {
		fs.UploadTaskManager.SetWorkersNumActive(taskFilterNegative(setting.GetInt(conf.TaskUploadThreadsNum, conf.Conf.Tasks.Upload.Workers)))
	})
	fs.ReconcileUploadPersistTempDir()
	copyOpts := []tache.Option{tache.WithWorks(setting.GetInt(conf.TaskCopyThreadsNum, conf.Conf.Tasks.Copy.Workers)), tache.WithPersistFunction(db.GetTaskDataFunc("copy", conf.Conf.Tasks.Copy.TaskPersistant), db.UpdateTaskDataFunc("copy", conf.Conf.Tasks.Copy.TaskPersistant)), tache.WithMaxRetry(conf.Conf.Tasks.Copy.MaxRetry), tache.WithRunning(false)}
	task.AssertPersistDebounceAsync(copyOpts...)
	fs.CopyTaskManager = tache.NewManager[*fs.CopyTask](copyOpts...)
	op.RegisterSettingChangingCallback(func() {
		fs.CopyTaskManager.SetWorkersNumActive(taskFilterNegative(setting.GetInt(conf.TaskCopyThreadsNum, conf.Conf.Tasks.Copy.Workers)))
	})
	// move reuses the copy task manager's worker/retry/persist configuration
	// (no separate move conf); it's a distinct manager since it holds *fs.MoveTask.
	moveOpts := []tache.Option{tache.WithWorks(setting.GetInt(conf.TaskCopyThreadsNum, conf.Conf.Tasks.Copy.Workers)), tache.WithPersistFunction(db.GetTaskDataFunc("move", conf.Conf.Tasks.Copy.TaskPersistant), db.UpdateTaskDataFunc("move", conf.Conf.Tasks.Copy.TaskPersistant)), tache.WithMaxRetry(conf.Conf.Tasks.Copy.MaxRetry), tache.WithRunning(false)}
	task.AssertPersistDebounceAsync(moveOpts...)
	fs.MoveTaskManager = tache.NewManager[*fs.MoveTask](moveOpts...)
	op.RegisterSettingChangingCallback(func() {
		fs.MoveTaskManager.SetWorkersNumActive(taskFilterNegative(setting.GetInt(conf.TaskCopyThreadsNum, conf.Conf.Tasks.Copy.Workers)))
	})
	CleanTempDir()
	go startTaskManagersOnStoragesLoaded(
		taskFilterNegative(setting.GetInt(conf.TaskUploadThreadsNum, conf.Conf.Tasks.Upload.Workers)),
		taskFilterNegative(setting.GetInt(conf.TaskCopyThreadsNum, conf.Conf.Tasks.Copy.Workers)),
	)
}

// startTaskManagersOnStoragesLoaded blocks (in its own goroutine, so it
// never delays server startup -- the same tradeoff LoadStorages itself
// already makes) until every storage has finished loading, then starts the
// three task managers InitTaskManager built paused. Polling
// conf.StoragesLoaded matches how the rest of the codebase observes storage
// readiness (see server/middlewares.StoragesLoaded); there's no existing
// signal/channel for "storages finished loading" to select on instead.
func startTaskManagersOnStoragesLoaded(uploadWorkers, copyWorkers int64) {
	for !conf.StoragesLoaded {
		time.Sleep(50 * time.Millisecond)
	}
	startPausedManager(fs.UploadTaskManager, uploadWorkers)
	startPausedManager(fs.CopyTaskManager, copyWorkers)
	startPausedManager(fs.MoveTaskManager, copyWorkers)
}

// startPausedManager starts a manager built with tache.WithRunning(false)
// and gets it back up to full configured concurrency, not just one task at
// a time. Manager.Start sets running=true and calls its (unexported) next()
// exactly once, which claims a single worker and dispatches a single queued
// task; every other worker sits idle until that one task's own completion
// chains into the next next() call, and so on -- so naively calling Start
// alone on a manager recovered with many queued tasks would silently cap it
// at concurrency 1 forever (nothing else ever calls next() enough times to
// claim the rest of the pool). Dropping active workers to 0 first makes
// Start's single dispatch attempt a no-op.
//
// Bumping straight to the target with one SetWorkersNumActive(active) call
// mostly works -- that's the same call
// op.RegisterSettingChangingCallback above already relies on -- but its loop
// (in the vendored tache library) re-checks the *live* queue length on every
// iteration, which shrinks by one each time it dispatches a task. When the
// jump is large relative to how many tasks are actually queued (recovering
// with fewer pending tasks than workers, or draining near the end of a
// batch), that shrinking bound trips early and silently under-dispatches.
// Requesting the increase one worker at a time instead keeps each call's
// jump at exactly 1, which the shrinking bound can never cut short as long
// as at least one task remains queued.
func startPausedManager[T tache.Task](m *tache.Manager[T], active int64) {
	m.SetWorkersNumActive(0)
	m.Start()
	for i := int64(1); i <= active; i++ {
		m.SetWorkersNumActive(i)
	}
}
