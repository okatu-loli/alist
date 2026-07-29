package bootstrap

import (
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/offline_download"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/internal/task"
	"github.com/xhofe/tache"
)

// InitOfflineDownloadManager builds the offline-download task manager and,
// like InitTaskManager's managers, starts it PAUSED: recovery loads and queues
// any persisted DownloadTasks immediately, but they only actually run once
// conf.StoragesLoaded is true (see startOfflineDownloadManagerOnStoragesLoaded).
// A recovered DownloadTask re-resolves its destination storage by mount path
// the moment it runs, so dispatching it before every storage has finished
// registering would race storage loading and burn the retry budget into
// StateFailed -- the same hazard the paused-then-started dance in
// InitTaskManager exists to avoid.
//
// It runs after InitTaskManager (which does CleanTempDir and kicks off storage
// loading) -- see cmd/server.go.
// defaultTransferWorkers is the fallback worker count for the transfer manager
// when offline_download_transfer_threads_num is unset (the setting seeds "5").
const defaultTransferWorkers = 5

func InitOfflineDownloadManager() {
	downloadOpts := []tache.Option{
		tache.WithWorks(setting.GetInt(conf.TaskOfflineDownloadThreadsNum, conf.Conf.Tasks.Download.Workers)),
		tache.WithPersistFunction(
			db.GetTaskDataFunc("offline_download", conf.Conf.Tasks.Download.TaskPersistant),
			db.UpdateTaskDataFunc("offline_download", conf.Conf.Tasks.Download.TaskPersistant),
		),
		tache.WithMaxRetry(conf.Conf.Tasks.Download.MaxRetry),
		tache.WithRunning(false),
	}
	// task.AssertPersistDebounceAsync guards the invariant TaskExtension's
	// locked Set* methods depend on (see its doc comment in
	// internal/task/base.go's mu field) -- see the matching call in
	// InitTaskManager for the full rationale.
	task.AssertPersistDebounceAsync(downloadOpts...)
	offline_download.OfflineDownloadTaskManager = tache.NewManager[*offline_download.DownloadTask](downloadOpts...)
	// The transfer manager mirrors the download manager: paused-until-StoragesLoaded
	// (a TransferTask re-resolves its destination storage by mount path the moment
	// it runs, so it must not dispatch before storage loading finishes), its own
	// persist key, and a settings-change callback that live-adjusts its worker count.
	transferOpts := []tache.Option{
		tache.WithWorks(setting.GetInt(conf.OfflineDownloadTransferThreadsNum, defaultTransferWorkers)),
		tache.WithPersistFunction(
			db.GetTaskDataFunc("offline_download_transfer", conf.Conf.Tasks.Download.TaskPersistant),
			db.UpdateTaskDataFunc("offline_download_transfer", conf.Conf.Tasks.Download.TaskPersistant),
		),
		tache.WithMaxRetry(conf.Conf.Tasks.Download.MaxRetry),
		tache.WithRunning(false),
	}
	task.AssertPersistDebounceAsync(transferOpts...)
	offline_download.TransferTaskManager = tache.NewManager[*offline_download.TransferTask](transferOpts...)
	op.RegisterSettingChangingCallback(func() {
		offline_download.OfflineDownloadTaskManager.SetWorkersNumActive(
			taskFilterNegative(setting.GetInt(conf.TaskOfflineDownloadThreadsNum, conf.Conf.Tasks.Download.Workers)),
		)
		offline_download.TransferTaskManager.SetWorkersNumActive(
			taskFilterNegative(setting.GetInt(conf.OfflineDownloadTransferThreadsNum, defaultTransferWorkers)),
		)
	})
	go startOfflineDownloadManagerOnStoragesLoaded(
		taskFilterNegative(setting.GetInt(conf.TaskOfflineDownloadThreadsNum, conf.Conf.Tasks.Download.Workers)),
		taskFilterNegative(setting.GetInt(conf.OfflineDownloadTransferThreadsNum, defaultTransferWorkers)),
	)
}

// startOfflineDownloadManagerOnStoragesLoaded mirrors
// startTaskManagersOnStoragesLoaded: it blocks in its own goroutine until every
// storage has finished loading, then brings the paused manager up to its full
// configured concurrency via startPausedManager.
func startOfflineDownloadManagerOnStoragesLoaded(workers, transferWorkers int64) {
	for !conf.StoragesLoaded {
		time.Sleep(50 * time.Millisecond)
	}
	startPausedManager(offline_download.OfflineDownloadTaskManager, workers)
	startPausedManager(offline_download.TransferTaskManager, transferWorkers)
}
