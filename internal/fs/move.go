package fs

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/task"
	"github.com/pkg/errors"
	"github.com/xhofe/tache"
)

// Move task i18n keys. GetStatus stores one of these keys (not a rendered
// string) so the task list can render it in the viewer's locale.
const (
	moveName         = "task.move"
	moveStatusMoving = "task.move_status.moving"
)

type MoveTask struct {
	task.TaskExtension
	// statusMu guards Status: Run() (the tache worker goroutine) writes it
	// while GetStatus/GetStatusKey (server/handles/task.go's getTaskInfo,
	// called from an HTTP handler goroutine) read it concurrently -- a data
	// race under -race. Status is our own field (not tache's), so it's
	// synchronized here directly rather than through task.TaskExtension.
	statusMu     sync.RWMutex
	Status       string        `json:"-"` //don't save status to save space
	SrcObjPath   string        `json:"src_path"`
	DstDirPath   string        `json:"dst_path"`
	srcStorage   driver.Driver `json:"-"`
	dstStorage   driver.Driver `json:"-"`
	SrcStorageMp string        `json:"src_storage_mp"`
	DstStorageMp string        `json:"dst_storage_mp"`
	// DetachPerms is the perm_policy=detach option (spec 0-C): after the move
	// succeeds and the follow hook has run, delete the manual permission rows
	// the relocation carried into the moved subtree. Persisted so a resumed
	// task still detaches.
	DetachPerms bool `json:"detach_perms"`
}

// GetName returns the English rendering of the task name for logging and
// back-compat; the task list re-renders GetNameKey/GetNameArgs per viewer locale.
func (t *MoveTask) GetName() string {
	return i18n.T(i18n.DefaultLang, t.GetNameKey(), t.GetNameArgs()...)
}

func (t *MoveTask) GetNameKey() string {
	return moveName
}

func (t *MoveTask) GetNameArgs() []any {
	return []any{
		"src_storage", t.SrcStorageMp, "src_path", t.SrcObjPath,
		"dst_storage", t.DstStorageMp, "dst_path", t.DstDirPath,
	}
}

// setStatus writes Status under statusMu; see statusMu's doc comment.
func (t *MoveTask) setStatus(status string) {
	t.statusMu.Lock()
	defer t.statusMu.Unlock()
	t.Status = status
}

// GetStatus renders the stored status key in English; t.Status holds a key.
func (t *MoveTask) GetStatus() string {
	key := t.GetStatusKey()
	if key == "" {
		return ""
	}
	return i18n.T(i18n.DefaultLang, key)
}

func (t *MoveTask) GetStatusKey() string {
	t.statusMu.RLock()
	defer t.statusMu.RUnlock()
	return t.Status
}

func (t *MoveTask) GetStatusArgs() []any {
	return nil
}

// MarshalJSON overrides tache's default reflection-based marshal for exactly
// tache.Base's State/Retry/MaxRetry fields (json:"state"/"retry"/"max_retry"):
// tache.Manager.persist() (active by default -- see bootstrap.InitTaskManager,
// which wires MoveTaskManager through WithPersistFunction whenever
// conf.Conf.Tasks.Copy.TaskPersistant is on, the default) reflects over every
// exported field with no synchronization at all, racing this task's own
// worker goroutine calling SetState/SetRetry mid-Run -- a real race,
// independent of (and not fixed by) the TaskExtension method-shadow lock,
// since reflection never goes through those methods. The "type Alias"
// pattern below keeps every other field on the default path (Go's
// encoding/json resolves a same-tag conflict in favor of the shallower,
// explicitly-listed field, so State/Retry/MaxRetry here take precedence over
// the same-named fields promoted from *Alias -> TaskExtension -> tache.Base,
// while everything else still comes from *Alias unmodified) -- only the three
// fields that need a lock-synchronized snapshot are hand-written. See
// task.TaskExtension.PersistSnapshot.
func (t *MoveTask) MarshalJSON() ([]byte, error) {
	type Alias MoveTask
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

func (t *MoveTask) Run() error {
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
	// move only supports moving within a single storage; a mismatch here
	// means the mount layout changed between task creation and execution.
	if t.srcStorage.GetStorage() != t.dstStorage.GetStorage() {
		return errors.WithStack(errs.MoveBetweenTwoStorages)
	}
	t.setStatus(moveStatusMoving)
	// Capture the manual permission rows that will follow BEFORE the move: a
	// failed move must delete nothing, and capturing IDs up front survives a
	// driver that renames the object mid-move. The follow hook fires inside
	// op.Move; detach then deletes exactly the captured rows.
	var detachIDs []uint
	if t.DetachPerms {
		detachIDs, err = op.CaptureManualPermissionIDs(t.Ctx(), t.srcStorage, t.SrcObjPath)
		if err != nil {
			return errors.WithMessage(err, "failed to capture permissions to detach")
		}
	}
	if err := op.Move(t.Ctx(), t.srcStorage, t.SrcObjPath, t.DstDirPath); err != nil {
		return err
	}
	if t.DetachPerms {
		return op.DeleteDetachedPermissions(detachIDs)
	}
	return nil
}

var MoveTaskManager *tache.Manager[*MoveTask]

// moveAsTask always creates a MoveTask and queues it; move only supports
// moving within a single storage (op.Move), so unlike copy there is no
// same-storage synchronous shortcut to fall back to.
func moveAsTask(ctx context.Context, srcObjPath, dstDirPath string, detachPerms bool) (task.TaskExtensionInfo, error) {
	srcStorage, srcObjActualPath, err := op.GetStorageAndActualPath(srcObjPath)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get src storage")
	}
	dstStorage, dstDirActualPath, err := op.GetStorageAndActualPath(dstDirPath)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get dst storage")
	}
	if srcStorage.GetStorage() != dstStorage.GetStorage() {
		return nil, errors.WithStack(errs.MoveBetweenTwoStorages)
	}
	taskCreator, _ := ctx.Value(ctxkey.User).(*model.User)
	t := &MoveTask{
		TaskExtension: task.TaskExtension{
			Creator: taskCreator,
		},
		srcStorage:   srcStorage,
		dstStorage:   dstStorage,
		SrcObjPath:   srcObjActualPath,
		DstDirPath:   dstDirActualPath,
		SrcStorageMp: srcStorage.GetStorage().MountPath,
		DstStorageMp: dstStorage.GetStorage().MountPath,
		DetachPerms:  detachPerms,
	}
	MoveTaskManager.Add(t)
	return t, nil
}
