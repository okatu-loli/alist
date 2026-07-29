package fs

import (
	"context"

	log "github.com/sirupsen/logrus"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/task"
	"github.com/pkg/errors"
)

// the param named path of functions in this package is a mount path
// So, the purpose of this package is to convert mount path to actual path
// then pass the actual path to the op package

type ListArgs struct {
	Refresh bool
	NoLog   bool
}

func List(ctx context.Context, path string, args *ListArgs) ([]model.Obj, error) {
	res, err := list(ctx, path, args)
	if err != nil {
		if !args.NoLog {
			log.Errorf("failed list %s: %+v", path, err)
		}
		return nil, err
	}
	return res, nil
}

type GetArgs struct {
	NoLog bool
}

func Get(ctx context.Context, path string, args *GetArgs) (model.Obj, error) {
	res, err := get(ctx, path)
	if err != nil {
		if !args.NoLog {
			log.Warnf("failed get %s: %s", path, err)
		}
		return nil, err
	}
	return res, nil
}

func Link(ctx context.Context, path string, args model.LinkArgs) (*model.Link, model.Obj, error) {
	res, file, err := link(ctx, path, args)
	if err != nil {
		log.Errorf("failed link %s: %+v", path, err)
		return nil, nil, err
	}
	return res, file, nil
}

func MakeDir(ctx context.Context, path string, lazyCache ...bool) error {
	err := makeDir(ctx, path, lazyCache...)
	if err != nil {
		log.Errorf("failed make dir %s: %+v", path, err)
		return err
	}
	emitFSEvent(ctx, ext.EventFSMkdir, path, "")
	return nil
}

func Move(ctx context.Context, srcPath, dstDirPath string, lazyCache ...bool) error {
	err := move(ctx, srcPath, dstDirPath, lazyCache...)
	if err != nil {
		log.Errorf("failed move %s to %s: %+v", srcPath, dstDirPath, err)
		return err
	}
	emitFSEvent(ctx, ext.EventFSMove, srcPath, dstDirPath)
	return nil
}

// MoveAsTask always queues a background MoveTask (move only supports moving
// within a single storage, so there's no synchronous same-storage shortcut).
// detachPerms carries the perm_policy=detach option down to the task.
func MoveAsTask(ctx context.Context, srcPath, dstDirPath string, detachPerms bool) (task.TaskExtensionInfo, error) {
	t, err := moveAsTask(ctx, srcPath, dstDirPath, detachPerms)
	if err != nil {
		log.Errorf("failed move %s to %s: %+v", srcPath, dstDirPath, err)
	}
	return t, err
}

// Copy copies srcObjPath into dstDirPath. clonePerms carries the copy_perms
// option: when true, the source subtree's manual permissions (with their role
// bindings) are cloned onto the destination after the copy succeeds.
func Copy(ctx context.Context, srcObjPath, dstDirPath string, clonePerms bool, lazyCache ...bool) (task.TaskExtensionInfo, error) {
	res, err := _copy(ctx, srcObjPath, dstDirPath, clonePerms, lazyCache...)
	if err != nil {
		log.Errorf("failed copy %s to %s: %+v", srcObjPath, dstDirPath, err)
	}
	return res, err
}

func Rename(ctx context.Context, srcPath, dstName string, lazyCache ...bool) error {
	err := rename(ctx, srcPath, dstName, lazyCache...)
	if err != nil {
		log.Errorf("failed rename %s to %s: %+v", srcPath, dstName, err)
		return err
	}
	emitFSEvent(ctx, ext.EventFSRename, srcPath, dstName)
	return nil
}

func Remove(ctx context.Context, path string) error {
	err := remove(ctx, path)
	if err != nil {
		log.Errorf("failed remove %s: %+v", path, err)
		return err
	}
	emitFSEvent(ctx, ext.EventFSRemove, path, "")
	return nil
}

func PutDirectly(ctx context.Context, dstDirPath string, file model.FileStreamer, lazyCache ...bool) error {
	err := putDirectly(ctx, dstDirPath, file, lazyCache...)
	if err != nil {
		log.Errorf("failed put %s: %+v", dstDirPath, err)
		return err
	}
	emitFSEvent(ctx, ext.EventFSPut, dstDirPath, "")
	return nil
}

func PutAsTask(ctx context.Context, dstDirPath string, file model.FileStreamer) (task.TaskExtensionInfo, error) {
	t, err := putAsTask(ctx, dstDirPath, file)
	if err != nil {
		log.Errorf("failed put %s: %+v", dstDirPath, err)
	}
	return t, err
}

type GetStoragesArgs struct {
}

func GetStorage(path string, args *GetStoragesArgs) (driver.Driver, error) {
	storageDriver, _, err := op.GetStorageAndActualPath(path)
	if err != nil {
		return nil, err
	}
	return storageDriver, nil
}

func Other(ctx context.Context, args model.FsOtherArgs) (interface{}, error) {
	res, err := other(ctx, args)
	if err != nil {
		log.Errorf("failed remove %s: %+v", args.Path, err)
	}
	return res, err
}

func PutURL(ctx context.Context, path, dstName, urlStr string) error {
	storage, dstDirActualPath, err := op.GetStorageAndActualPath(path)
	if err != nil {
		return errors.WithMessage(err, "failed get storage")
	}
	if storage.Config().NoUpload {
		return errors.WithStack(errs.UploadNotSupported)
	}
	_, ok := storage.(driver.PutURL)
	_, okResult := storage.(driver.PutURLResult)
	if !ok && !okResult {
		return errs.NotImplement
	}
	return op.PutURL(ctx, storage, dstDirActualPath, dstName, urlStr)
}
