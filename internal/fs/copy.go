package fs

import (
	"context"
	"encoding/json"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/i18n"
	"net/http"
	stdpath "path"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/internal/task"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
	"github.com/xhofe/tache"
)

// Copy task i18n keys. GetStatus stores one of these keys (not a rendered
// string) so the task list can render it in the viewer's locale.
const (
	copyName             = "task.copy"
	copyStatusGettingSrc = "task.copy_status.getting_src"
	copyStatusListingSrc = "task.copy_status.listing_src"
	copyStatusDirQueued  = "task.copy_status.dir_queued"
)

type CopyTask struct {
	task.TaskExtension
	// statusMu guards Status: Run() (via copyBetween2Storages, on the tache
	// worker goroutine) writes it while GetStatus/GetStatusKey
	// (server/handles/task.go's getTaskInfo, called from an HTTP handler
	// goroutine) read it concurrently -- a data race under -race. Status is
	// our own field (not tache's), so it's synchronized here directly rather
	// than through task.TaskExtension.
	statusMu     sync.RWMutex
	Status       string        `json:"-"` //don't save status to save space
	SrcObjPath   string        `json:"src_path"`
	DstDirPath   string        `json:"dst_path"`
	srcStorage   driver.Driver `json:"-"`
	dstStorage   driver.Driver `json:"-"`
	SrcStorageMp string        `json:"src_storage_mp"`
	DstStorageMp string        `json:"dst_storage_mp"`
	// ClonePerms is the copy_perms=true option (spec 0-C): after the copy
	// succeeds, clone the source subtree's manual permission rows (with their
	// role bindings) onto the destination. Set only on the top-level task the
	// request creates — the recursive child tasks copyBetween2Storages queues
	// leave it false so the whole subtree is cloned exactly once. Persisted so
	// a resumed task still clones.
	ClonePerms bool `json:"clone_perms"`
}

// GetName returns the English rendering of the task name for logging and
// back-compat; the task list re-renders GetNameKey/GetNameArgs per viewer locale.
func (t *CopyTask) GetName() string {
	return i18n.T(i18n.DefaultLang, t.GetNameKey(), t.GetNameArgs()...)
}

func (t *CopyTask) GetNameKey() string {
	return copyName
}

func (t *CopyTask) GetNameArgs() []any {
	return []any{
		"src_storage", t.SrcStorageMp, "src_path", t.SrcObjPath,
		"dst_storage", t.DstStorageMp, "dst_path", t.DstDirPath,
	}
}

// setStatus writes Status under statusMu; see statusMu's doc comment.
func (t *CopyTask) setStatus(status string) {
	t.statusMu.Lock()
	defer t.statusMu.Unlock()
	t.Status = status
}

// GetStatus renders the stored status key in English; t.Status holds a key.
func (t *CopyTask) GetStatus() string {
	key := t.GetStatusKey()
	if key == "" {
		return ""
	}
	return i18n.T(i18n.DefaultLang, key)
}

func (t *CopyTask) GetStatusKey() string {
	t.statusMu.RLock()
	defer t.statusMu.RUnlock()
	return t.Status
}

func (t *CopyTask) GetStatusArgs() []any {
	return nil
}

// MarshalJSON overrides tache's default reflection-based marshal for exactly
// tache.Base's State/Retry/MaxRetry fields; see MoveTask.MarshalJSON (same
// mechanism, same hazard: tache.Manager.persist() reflects over these fields
// unsynchronized, on by default via conf.Conf.Tasks.Copy.TaskPersistant).
func (t *CopyTask) MarshalJSON() ([]byte, error) {
	type Alias CopyTask
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

func (t *CopyTask) Run() error {
	t.ReinitCtx()
	t.ClearEndTime()
	t.SetStartTime(time.Now())
	defer func() { t.SetEndTime(time.Now()) }()
	var err error
	if t.srcStorage == nil {
		t.srcStorage, err = op.GetStorageByMountPath(t.SrcStorageMp)
	}
	if t.dstStorage == nil {
		t.dstStorage, err = op.GetStorageByMountPath(t.DstStorageMp)
	}
	if err != nil {
		return errors.WithMessage(err, "failed get storage")
	}
	if err := copyBetween2Storages(t, t.srcStorage, t.dstStorage, t.SrcObjPath, t.DstDirPath); err != nil {
		return err
	}
	if t.ClonePerms {
		// Clone the whole source subtree's manual permissions in one shot: it is
		// path-based (reads the still-present source, re-anchors by arithmetic),
		// so it does not depend on the queued child copies having finished.
		dstRel := stdpath.Join(t.DstDirPath, stdpath.Base(t.SrcObjPath))
		return op.ClonePermissionsForCopy(t.Ctx(), t.srcStorage, t.SrcObjPath, t.dstStorage, dstRel)
	}
	return nil
}

var CopyTaskManager *tache.Manager[*CopyTask]

// Copy if in the same storage, call move method
// if not, add copy task
func _copy(ctx context.Context, srcObjPath, dstDirPath string, clonePerms bool, lazyCache ...bool) (task.TaskExtensionInfo, error) {
	srcStorage, srcObjActualPath, err := op.GetStorageAndActualPath(srcObjPath)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get src storage")
	}
	dstStorage, dstDirActualPath, err := op.GetStorageAndActualPath(dstDirPath)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get dst storage")
	}
	// copy if in the same storage, just call driver.Copy
	if srcStorage.GetStorage() == dstStorage.GetStorage() {
		err = op.Copy(ctx, srcStorage, srcObjActualPath, dstDirActualPath, lazyCache...)
		if !errors.Is(err, errs.NotImplement) && !errors.Is(err, errs.NotSupport) {
			if err == nil && clonePerms {
				// Same-storage copy completed synchronously: clone the source
				// subtree's manual permissions onto the destination now.
				dstRel := stdpath.Join(dstDirActualPath, stdpath.Base(srcObjActualPath))
				err = op.ClonePermissionsForCopy(ctx, srcStorage, srcObjActualPath, dstStorage, dstRel)
			}
			return nil, err
		}
	}
	// not in the same storage
	taskCreator, _ := ctx.Value(ctxkey.User).(*model.User)
	t := &CopyTask{
		TaskExtension: task.TaskExtension{
			Creator: taskCreator,
		},
		srcStorage:   srcStorage,
		dstStorage:   dstStorage,
		SrcObjPath:   srcObjActualPath,
		DstDirPath:   dstDirActualPath,
		SrcStorageMp: srcStorage.GetStorage().MountPath,
		DstStorageMp: dstStorage.GetStorage().MountPath,
		ClonePerms:   clonePerms,
	}
	CopyTaskManager.Add(t)
	return t, nil
}

func copyBetween2Storages(t *CopyTask, srcStorage, dstStorage driver.Driver, srcObjPath, dstDirPath string) error {
	t.setStatus(copyStatusGettingSrc)
	srcObj, err := op.Get(t.Ctx(), srcStorage, srcObjPath)
	if err != nil {
		return errors.WithMessagef(err, "failed get src [%s] file", srcObjPath)
	}
	if srcObj.IsDir() {
		t.setStatus(copyStatusListingSrc)
		objs, err := op.List(t.Ctx(), srcStorage, srcObjPath, model.ListArgs{})
		if err != nil {
			return errors.WithMessagef(err, "failed list src [%s] objs", srcObjPath)
		}
		for _, obj := range objs {
			if utils.IsCanceled(t.Ctx()) {
				return nil
			}
			srcObjPath := stdpath.Join(srcObjPath, obj.GetName())
			dstObjPath := stdpath.Join(dstDirPath, srcObj.GetName())
			CopyTaskManager.Add(&CopyTask{
				TaskExtension: task.TaskExtension{
					Creator: t.GetCreator(),
				},
				srcStorage:   srcStorage,
				dstStorage:   dstStorage,
				SrcObjPath:   srcObjPath,
				DstDirPath:   dstObjPath,
				SrcStorageMp: srcStorage.GetStorage().MountPath,
				DstStorageMp: dstStorage.GetStorage().MountPath,
			})
		}
		t.setStatus(copyStatusDirQueued)
		return nil
	}
	return copyFileBetween2Storages(t, srcStorage, dstStorage, srcObjPath, dstDirPath)
}

func copyFileBetween2Storages(tsk *CopyTask, srcStorage, dstStorage driver.Driver, srcFilePath, dstDirPath string) error {
	srcFile, err := op.Get(tsk.Ctx(), srcStorage, srcFilePath)
	if err != nil {
		return errors.WithMessagef(err, "failed get src [%s] file", srcFilePath)
	}
	tsk.SetTotalBytes(srcFile.GetSize())
	link, _, err := op.Link(tsk.Ctx(), srcStorage, srcFilePath, model.LinkArgs{
		Header: http.Header{},
	})
	if err != nil {
		return errors.WithMessagef(err, "failed get [%s] link", srcFilePath)
	}
	fs := stream.FileStream{
		Obj: srcFile,
		Ctx: tsk.Ctx(),
	}
	// any link provided is seekable
	ss, err := stream.NewSeekableStream(fs, link)
	if err != nil {
		return errors.WithMessagef(err, "failed get [%s] stream", srcFilePath)
	}
	return op.Put(tsk.Ctx(), dstStorage, dstDirPath, ss, tsk.SetProgress, true)
}
