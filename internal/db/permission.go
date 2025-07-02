package db

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
)

func GetPermissionById(id uint) (*model.Permission, error) {
	var p model.Permission
	if err := db.First(&p, id).Error; err != nil {
		return nil, errors.Wrapf(err, "failed get old permission")
	}
	return &p, nil
}

func GetPermissionByName(name string) (*model.Permission, error) {
	p := model.Permission{Name: name}
	if err := db.Where(p).First(&p).Error; err != nil {
		return nil, errors.Wrapf(err, "failed find permission")
	}
	return &p, nil
}

func CreatePermission(p *model.Permission) error {
	return errors.WithStack(db.Create(p).Error)
}

func UpdatePermission(p *model.Permission) error {
	return errors.WithStack(db.Save(p).Error)
}

func GetPermissions(pageIndex, pageSize int) (ps []model.Permission, count int64, err error) {
	pDB := db.Model(&model.Permission{})
	if err = pDB.Count(&count).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get permissions count")
	}
	if err = pDB.Order(columnName("id")).Offset((pageIndex - 1) * pageSize).Limit(pageSize).Find(&ps).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get find permissions")
	}
	return ps, count, nil
}

func DeletePermissionById(id uint) error {
	return errors.WithStack(db.Delete(&model.Permission{}, id).Error)
}
