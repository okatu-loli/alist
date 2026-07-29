package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/internal/task"
	"github.com/pkg/errors"
	"github.com/xhofe/tache"
)

// Upload task i18n keys; the task list renders these per viewer locale.
const (
	uploadName            = "task.upload"
	uploadStatusUploading = "task.upload_status.uploading"
)

// UploadPersistTempDirName is the subdirectory of conf.Conf.TempDir that
// holds temp files backing resumable (persisted) upload tasks.
// bootstrap.CleanTempDir excludes it from its blanket wipe on startup, since
// those files are exactly what a resumed UploadTask reads from; instead
// ReconcileUploadPersistTempDir prunes it once UploadTaskManager has
// recovered whatever tasks survived the restart.
const UploadPersistTempDirName = "upload-persist"

type UploadTask struct {
	task.TaskExtension
	// storage/file are live handles used while this task runs in the current
	// process. They're unexported (excluded from JSON) and nil until Run()
	// populates them -- either immediately in putAsTask (fresh task) or
	// lazily inside Run() itself (task resumed from persisted DB state,
	// see rebuildFromPersisted). GetName/GetStatus/GetNameArgs must not
	// depend on these being set: a recovered-but-not-yet-run task can be
	// listed/rendered before Run() ever executes.
	storage driver.Driver
	file    model.FileStreamer

	// Persistable fields, always populated at task creation (putAsTask) so
	// the task can be named/rendered and, if TempFilePath is set, resumed
	// after a restart without depending on the live handles above.
	DstStorageMp     string    `json:"dst_storage_mp"`
	DstDirActualPath string    `json:"dst_dir_actual_path"`
	FileName         string    `json:"file_name"`
	FileSize         int64     `json:"file_size"`
	FileModified     time.Time `json:"file_modified"`
	FileMimetype     string    `json:"file_mimetype"`
	// TempFilePath is the on-disk path (under
	// <TempDir>/UploadPersistTempDirName) of this upload's fully-cached
	// content. It's only set when TaskConfig.Upload.TaskPersistant is
	// enabled and the content was disk-backed (see persistableTempFilePath);
	// an empty value means this task cannot be resumed after a restart.
	TempFilePath string `json:"temp_file_path,omitempty"`
}

// GetName returns the English rendering for logging/back-compat; the task list
// re-renders GetNameKey/GetNameArgs per viewer locale.
func (t *UploadTask) GetName() string {
	return i18n.T(i18n.DefaultLang, t.GetNameKey(), t.GetNameArgs()...)
}

func (t *UploadTask) GetNameKey() string {
	return uploadName
}

func (t *UploadTask) GetNameArgs() []any {
	return []any{
		"name", t.FileName,
		"dst_storage", t.DstStorageMp,
		"dst_path", t.DstDirActualPath,
	}
}

func (t *UploadTask) GetStatus() string {
	return i18n.T(i18n.DefaultLang, uploadStatusUploading)
}

func (t *UploadTask) GetStatusKey() string {
	return uploadStatusUploading
}

func (t *UploadTask) GetStatusArgs() []any {
	return nil
}

// MarshalJSON overrides tache's default reflection-based marshal for exactly
// tache.Base's State/Retry/MaxRetry fields; see MoveTask.MarshalJSON (same
// mechanism, same hazard: tache.Manager.persist() reflects over these fields
// unsynchronized, on by default via conf.Conf.Tasks.Upload.TaskPersistant).
// UploadTask's own fields (DstStorageMp, FileName, TempFilePath, ...) are all
// populated once in putAsTask before UploadTaskManager.Add(t) makes the task
// visible to persist()'s GetAll() -- never mutated after -- so they don't
// need the same treatment.
func (t *UploadTask) MarshalJSON() ([]byte, error) {
	type Alias UploadTask
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

func (t *UploadTask) Run() error {
	t.ClearEndTime()
	t.SetStartTime(time.Now())
	defer func() { t.SetEndTime(time.Now()) }()
	if err := t.rebuildFromPersisted(); err != nil {
		return err
	}
	return op.Put(t.Ctx(), t.storage, t.DstDirActualPath, t.file, t.SetProgress, true)
}

// rebuildFromPersisted resolves the live storage/file handles when they're
// missing -- which only happens for a task recovered from the persisted DB
// state after a restart (a freshly created task already has both set by
// putAsTask). It re-resolves storage the same way CopyTask/MoveTask do
// (op.GetStorageByMountPath, since storages are loaded before
// bootstrap.InitTaskManager recovers tasks) and reopens the cached temp
// file recorded in TempFilePath.
func (t *UploadTask) rebuildFromPersisted() error {
	if t.storage == nil {
		storage, err := op.GetStorageByMountPath(t.DstStorageMp)
		if err != nil {
			return errors.WithMessage(err, "failed get storage")
		}
		t.storage = storage
	}
	if t.file == nil {
		if t.TempFilePath == "" {
			return errors.New("upload task has no persisted temp file to resume from")
		}
		f, err := os.Open(t.TempFilePath)
		if err != nil {
			return errors.WithMessagef(err, "failed to reopen temp file [%s] to resume upload", t.TempFilePath)
		}
		fs := &stream.FileStream{
			Obj: &model.Object{
				Name:     t.FileName,
				Size:     t.FileSize,
				Modified: t.FileModified,
			},
			Mimetype: t.FileMimetype,
		}
		fs.SetTmpFile(f)
		t.file = fs
	}
	return nil
}

var UploadTaskManager *tache.Manager[*UploadTask]

// putAsTask add as a put task and return immediately
func putAsTask(ctx context.Context, dstDirPath string, file model.FileStreamer) (task.TaskExtensionInfo, error) {
	storage, dstDirActualPath, err := op.GetStorageAndActualPath(dstDirPath)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get storage")
	}
	if storage.Config().NoUpload {
		return nil, errors.WithStack(errs.UploadNotSupported)
	}
	if file.NeedStore() {
		_, err := file.CacheFullInTempFile()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create temp file")
		}
		//file.SetReader(tempFile)
		//file.SetTmpFile(tempFile)
	}
	taskCreator, _ := ctx.Value(ctxkey.User).(*model.User) // taskCreator is nil when convert failed
	t := &UploadTask{
		TaskExtension: task.TaskExtension{
			Creator: taskCreator,
		},
		storage:          storage,
		file:             file,
		DstStorageMp:     storage.GetStorage().MountPath,
		DstDirActualPath: dstDirActualPath,
		FileName:         file.GetName(),
		FileSize:         file.GetSize(),
		FileModified:     file.ModTime(),
		FileMimetype:     file.GetMimetype(),
	}
	t.SetTotalBytes(file.GetSize())
	if conf.Conf.Tasks.Upload.TaskPersistant {
		if tempPath, ok := persistableTempFilePath(file); ok {
			t.TempFilePath = tempPath
		}
	}
	UploadTaskManager.Add(t)
	return t, nil
}

// persistableTempFilePath relocates file's already-cached temp file (see
// stream.FileStream.CacheFullInTempFile, called by putAsTask above whenever
// file.NeedStore() -- always true for task-based uploads, see
// server/handles/fsup.go) into <TempDir>/UploadPersistTempDirName so it
// survives bootstrap.CleanTempDir's startup wipe, and returns its new path.
//
// It only takes over a file that's genuinely disk-backed under
// conf.Conf.TempDir: file.GetFile() must be a concrete *os.File (not just
// something implementing model.File) rooted under TempDir, which is exactly
// what utils.CreateTempFile produces and is the only way CacheFullInTempFile
// is reached from the upload handlers today. Anything else (no cached file,
// or a caller-owned file outside TempDir) is left untouched and simply won't
// be resumable -- ok returns false and the task falls back to
// non-persistable behavior (temp file deleted on completion like before,
// wiped by CleanTempDir if the process dies first).
func persistableTempFilePath(file model.FileStreamer) (path string, ok bool) {
	osFile, isOSFile := file.GetFile().(*os.File)
	if !isOSFile {
		return "", false
	}
	oldPath := osFile.Name()
	rel, err := filepath.Rel(conf.Conf.TempDir, oldPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	persistDir := filepath.Join(conf.Conf.TempDir, UploadPersistTempDirName)
	if err := os.MkdirAll(persistDir, 0o777); err != nil {
		return "", false
	}
	newPath := filepath.Join(persistDir, filepath.Base(oldPath))
	if err := os.Rename(oldPath, newPath); err != nil {
		return "", false
	}
	// FileStream.Close() deletes whatever f.tmpFile.Name() reports; that's
	// still oldPath until we hand it a handle opened at the new location.
	newHandle, err := os.Open(newPath)
	if err != nil {
		_ = os.Rename(newPath, oldPath) // best-effort rollback
		return "", false
	}
	file.SetTmpFile(newHandle)
	return newPath, true
}

// ReconcileUploadPersistTempDir deletes any file under
// <TempDir>/UploadPersistTempDirName that isn't referenced by a currently
// loaded UploadTask. It must run after UploadTaskManager has been
// constructed (so it has recovered any persisted tasks and their
// TempFilePath fields are known) -- see bootstrap.InitTaskManager. This is
// what keeps the directory from growing unbounded: tasks that are
// canceled/removed before they ever run, or a crash between persisting a
// task and it completing, leave an orphaned temp file behind that
// bootstrap.CleanTempDir deliberately does not touch; this sweeps those on
// the next startup instead.
func ReconcileUploadPersistTempDir() {
	dir := filepath.Join(conf.Conf.TempDir, UploadPersistTempDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keep := make(map[string]struct{}, len(entries))
	if UploadTaskManager != nil {
		for _, t := range UploadTaskManager.GetAll() {
			if t.TempFilePath != "" {
				keep[filepath.Base(t.TempFilePath)] = struct{}{}
			}
		}
	}
	for _, entry := range entries {
		if _, ok := keep[entry.Name()]; ok {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
	}
}

// putDirect put the file and return after finish
func putDirectly(ctx context.Context, dstDirPath string, file model.FileStreamer, lazyCache ...bool) error {
	storage, dstDirActualPath, err := op.GetStorageAndActualPath(dstDirPath)
	if err != nil {
		return errors.WithMessage(err, "failed get storage")
	}
	if storage.Config().NoUpload {
		return errors.WithStack(errs.UploadNotSupported)
	}
	return op.Put(ctx, storage, dstDirActualPath, file, nil, lazyCache...)
}
