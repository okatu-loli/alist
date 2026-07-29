package offline_download

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	stdpath "path"
	"path/filepath"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/internal/task"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/xhofe/tache"
)

// DeletePolicy controls whether a TransferTask's local temp source is removed
// after the transfer, and on which outcome. Values mirror v3 exactly so a
// persisted task JSON round-trips across the v3→v4 boundary unchanged.
type DeletePolicy string

const (
	DeleteOnUploadSucceed DeletePolicy = "delete_on_upload_succeed"
	DeleteOnUploadFailed  DeletePolicy = "delete_on_upload_failed"
	DeleteNever           DeletePolicy = "delete_never"
	DeleteAlways          DeletePolicy = "delete_always"
)

// TransferTask moves ONE completed local-temp file into a destination storage
// directory via op.Put. It is the v4 port of v3's TransferTask, reduced to the
// STD (local-temp→storage) path only: the source is always a plain file path on
// the local filesystem (SrcFilePath), never a cloud object, so there is no
// SrcStorage/driver here (the cloud transferObj path is out of scope for this
// wave). A daemon offline-download tool downloads into conf.TempDir/<tool>/<uuid>
// and, on completion, transferStd enumerates that tree and enqueues one
// TransferTask per regular file.
//
// Only SrcFilePath / DstStorageMp / DstDirPath / DeletePolicy / FileSize are
// JSON-persisted; the task is re-runnable from those fields alone (Run reopens
// the temp file and re-resolves the destination storage by mount path).
type TransferTask struct {
	task.TaskExtension
	// statusMu guards status: transferStdFile (the tache worker goroutine)
	// writes it while GetStatus reads it concurrently. TransferTaskManager
	// isn't wired to any HTTP route today (see SetupTaskRoute), so
	// getTaskInfo never actually calls GetStatus on a live TransferTask --
	// this is currently latent, not reachable -- but it's the exact same
	// hazard MoveTask.Status/CopyTask.Status had, so it's guarded the same
	// way rather than left as a trap for whenever TransferTaskManager does
	// get a route.
	statusMu sync.RWMutex
	// status is not persisted (it is transient progress text, regenerated each
	// run), matching v3's `json:"-"` on its Status field.
	status       string       `json:"-"`
	SrcFilePath  string       `json:"src_file_path"`
	DstStorageMp string       `json:"dst_storage_mp"`
	DstDirPath   string       `json:"dst_dir_path"`
	DeletePolicy DeletePolicy `json:"delete_policy"`
	FileSize     int64        `json:"file_size"`
}

// GetName renders a human-readable label for logs / the task list. TransferTask
// deliberately does NOT implement the optional TaskInfoWithI18n interface (no
// GetNameKey): unlike DownloadTask it is an internal plumbing task, so the plain
// English name is sufficient and the task-list handler falls back to it.
func (t *TransferTask) GetName() string {
	return fmt.Sprintf("transfer [%s] to [%s](%s)", t.SrcFilePath, t.DstStorageMp, t.DstDirPath)
}

// setStatus writes status under statusMu; see statusMu's doc comment.
func (t *TransferTask) setStatus(status string) {
	t.statusMu.Lock()
	defer t.statusMu.Unlock()
	t.status = status
}

func (t *TransferTask) GetStatus() string {
	t.statusMu.RLock()
	defer t.statusMu.RUnlock()
	return t.status
}

// MarshalJSON overrides tache's default reflection-based marshal for exactly
// tache.Base's State/Retry/MaxRetry fields; see MoveTask.MarshalJSON (same
// mechanism, same hazard: tache.Manager.persist() reflects over these fields
// unsynchronized, on by default via conf.Conf.Tasks.Download.TaskPersistant,
// which TransferTaskManager is also wired through -- see
// bootstrap.InitOfflineDownloadManager).
func (t *TransferTask) MarshalJSON() ([]byte, error) {
	type Alias TransferTask
	state, retry, maxRetry := t.PersistSnapshot()
	return json.Marshal(&struct {
		State    tache.State `json:"state"`
		Retry    int         `json:"retry"`
		MaxRetry int         `json:"max_retry"`
		*Alias
	}{
		State:    state,
		Retry:    retry,
		MaxRetry: maxRetry,
		Alias:    (*Alias)(t),
	})
}

// Run reopens the local temp file and streams it into the destination storage
// directory with op.Put. Safe to re-run after a restart: it re-resolves the
// destination storage by mount path and reopens SrcFilePath from scratch.
func (t *TransferTask) Run() error {
	t.ReinitCtx()
	t.ClearEndTime()
	t.SetStartTime(time.Now())
	defer func() { t.SetEndTime(time.Now()) }()
	return transferStdFile(t)
}

// OnSucceeded removes the temp source file when the policy calls for cleanup on
// a successful upload (DeleteOnUploadSucceed / DeleteAlways). Mirrors v3.
func (t *TransferTask) OnSucceeded() {
	if t.DeletePolicy == DeleteOnUploadSucceed || t.DeletePolicy == DeleteAlways {
		removeStdTemp(t)
	}
}

// OnFailed removes the temp source file when the policy calls for cleanup on a
// failed upload (DeleteOnUploadFailed / DeleteAlways). Mirrors v3.
func (t *TransferTask) OnFailed() {
	if t.DeletePolicy == DeleteOnUploadFailed || t.DeletePolicy == DeleteAlways {
		removeStdTemp(t)
	}
}

// TransferTaskManager runs TransferTasks. It is constructed (paused, started
// once storages finish loading) by bootstrap.InitOfflineDownloadManager, the
// same lifecycle as OfflineDownloadTaskManager.
var TransferTaskManager *tache.Manager[*TransferTask]

// transferStd walks the local temp tree rooted at tempDir and enqueues one
// TransferTask per regular file, preserving each file's sub-directory position
// relative to tempDir under dstDirPath (so conf.TempDir/<tool>/<uuid>/sub/f.bin
// lands at dstDirPath/sub/f.bin). This is the v4 port of v3's transferStd +
// transferStdPath directory recursion, done eagerly with a single filepath.Walk
// instead of v3's per-task lazy os.ReadDir recursion — the end mapping (files
// under dstDirPath preserving structure) is identical, but the enumeration all
// happens up front so the DownloadTask can Remove the daemon task immediately
// after enqueue.
//
// The destination storage is resolved by mount path (dstStorageMp is already the
// mount path and dstDirPath the actual path within it, both carried on the
// DownloadTask), so no op.GetStorageAndActualPath split is needed here.
//
// Temp cleanup ordering: transferStd only ENQUEUES; the TransferTasks run
// asynchronously afterward and read their source files, so this function must
// NOT delete any temp file or the temp dir (doing so would race the reads).
// Per-file removal happens later, per DeletePolicy, in each TransferTask's
// OnSucceeded/OnFailed once its upload is done; empty temp directories left
// behind are reclaimed by bootstrap's CleanTempDir on the next start — matching
// v3, which likewise never removes the temp tree in transferStd.
func transferStd(ctx context.Context, tempDir, dstStorageMp, dstDirPath string, deletePolicy DeletePolicy) error {
	if _, err := op.GetStorageByMountPath(dstStorageMp); err != nil {
		return errors.WithMessage(err, "failed get dst storage")
	}
	taskCreator, _ := ctx.Value(ctxkey.User).(*model.User)
	return filepath.Walk(tempDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		// Position the file under dstDirPath preserving its sub-directory path
		// relative to tempDir. rel for a top-level file is just its base name
		// (relDir == "."), so it lands directly in dstDirPath.
		rel, err := filepath.Rel(tempDir, path)
		if err != nil {
			return err
		}
		relDir := filepath.Dir(rel)
		dstDir := dstDirPath
		if relDir != "." {
			dstDir = stdpath.Join(dstDirPath, filepath.ToSlash(relDir))
		}
		TransferTaskManager.Add(&TransferTask{
			TaskExtension: task.TaskExtension{
				Creator: taskCreator,
			},
			SrcFilePath:  path,
			DstStorageMp: dstStorageMp,
			DstDirPath:   dstDir,
			DeletePolicy: deletePolicy,
			FileSize:     info.Size(),
		})
		return nil
	})
}

// transferStdFile is the body of TransferTask.Run: open the local temp file,
// wrap it in a FileStream, and op.Put it into the destination storage. It is the
// v4 port of v3's transferStdFile.
func transferStdFile(t *TransferTask) error {
	t.setStatus("getting src object")
	dstStorage, err := op.GetStorageByMountPath(t.DstStorageMp)
	if err != nil {
		return errors.WithMessage(err, "failed get dst storage")
	}
	rc, err := os.Open(t.SrcFilePath)
	if err != nil {
		return errors.Wrapf(err, "failed to open file %s", t.SrcFilePath)
	}
	info, err := rc.Stat()
	if err != nil {
		_ = rc.Close()
		return errors.Wrapf(err, "failed to stat file %s", t.SrcFilePath)
	}
	mimetype := utils.GetMimeType(t.SrcFilePath)
	s := &stream.FileStream{
		Obj: &model.Object{
			Name:     filepath.Base(t.SrcFilePath),
			Size:     info.Size(),
			Modified: info.ModTime(),
			IsFolder: false,
		},
		Reader:   rc,
		Mimetype: mimetype,
		Closers:  utils.NewClosers(rc),
	}
	t.SetTotalBytes(info.Size())
	t.setStatus("transferring")
	return op.Put(t.Ctx(), dstStorage, t.DstDirPath, s, t.SetProgress)
}

// removeStdTemp deletes a TransferTask's local temp source file, honoring the
// DeletePolicy decision made by the caller (OnSucceeded/OnFailed). A directory
// (should never happen for a STD file task) or a missing file is a no-op.
func removeStdTemp(t *TransferTask) {
	info, err := os.Stat(t.SrcFilePath)
	if err != nil || info.IsDir() {
		return
	}
	if err := os.Remove(t.SrcFilePath); err != nil {
		log.Errorf("failed to delete temp file %s, error: %s", t.SrcFilePath, err.Error())
	}
}
