package fs

import (
	"context"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// List files
func list(ctx context.Context, path string, args *ListArgs) ([]model.Obj, error) {
	virtualFiles := op.GetStorageVirtualFilesByPath(path)
	storage, actualPath, err := op.GetStorageAndActualPath(path)
	if err != nil && len(virtualFiles) == 0 {
		return nil, errors.WithMessage(err, "failed get storage")
	}

	var _objs []model.Obj
	if storage != nil {
		_objs, err = op.List(ctx, storage, actualPath, model.ListArgs{
			ReqPath: path,
			Refresh: args.Refresh,
		})
		if err != nil {
			if !args.NoLog {
				log.Errorf("fs/list: %+v", err)
			}
			if len(virtualFiles) == 0 {
				return nil, errors.WithMessage(err, "failed get objs")
			}
		}
	}

	om := model.NewObjMerge()
	objs := om.Merge(_objs, virtualFiles...)
	return objs, nil
}
