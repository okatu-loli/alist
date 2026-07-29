package db

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
)

func GetMetaByPath(path string) (*model.Meta, error) {
	meta := model.Meta{Path: path}
	if err := db.Where(meta).First(&meta).Error; err != nil {
		return nil, errors.Wrapf(err, "failed select meta")
	}
	return &meta, nil
}

func GetMetaById(id uint) (*model.Meta, error) {
	var u model.Meta
	if err := db.First(&u, id).Error; err != nil {
		return nil, errors.Wrapf(err, "failed get old meta")
	}
	return &u, nil
}

func CreateMeta(u *model.Meta) error {
	return errors.WithStack(db.Create(u).Error)
}

func UpdateMeta(u *model.Meta) error {
	return errors.WithStack(db.Save(u).Error)
}

// GetAllMetas loads every meta row. The meta table is small (one row per
// admin-configured directory), so the process-wide meta index (op.loadMetaIndex)
// derives paths from a single full scan rather than per-path DB lookups.
func GetAllMetas() ([]model.Meta, error) {
	var metas []model.Meta
	if err := db.Find(&metas).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return metas, nil
}

// GetNodelessMetas returns meta rows not yet anchored to a node (node_id 0):
// above-mount virtual paths and rows carried in by a v3→v4 migration awaiting
// first-load adoption (op.adoptNodelessAnchors).
func GetNodelessMetas() ([]model.Meta, error) {
	var metas []model.Meta
	if err := db.Where("node_id = 0 OR node_id IS NULL").Find(&metas).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return metas, nil
}

// SetMetaNodeID backfills a meta's node_id (first-load adoption). Uses a scoped
// column update so the surrounding row is untouched.
func SetMetaNodeID(id, nodeID uint) error {
	return errors.WithStack(db.Model(&model.Meta{}).Where("id = ?", id).
		Update("node_id", nodeID).Error)
}

func GetMetas(pageIndex, pageSize int) (metas []model.Meta, count int64, err error) {
	metaDB := db.Model(&model.Meta{})
	if err = metaDB.Count(&count).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get metas count")
	}
	if err = metaDB.Order(columnName("id")).Offset((pageIndex - 1) * pageSize).Limit(pageSize).Find(&metas).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get find metas")
	}
	return metas, count, nil
}

func DeleteMetaById(id uint) error {
	return errors.WithStack(db.Delete(&model.Meta{}, id).Error)
}
