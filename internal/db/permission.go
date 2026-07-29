package db

import (
	"encoding/json"
	"fmt"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// rolePermissionsJoin builds the "JOIN role_permissions rp ON rp.permission_id
// = permissions.id" clause with both table names resolved through the live
// NamingStrategy, so it honors --table-prefix like every other query here
// instead of hardcoding the unprefixed names.
func rolePermissionsJoin() string {
	return fmt.Sprintf("JOIN %s rp ON rp.permission_id = %s.id",
		db.NamingStrategy.TableName("RolePermission"), db.NamingStrategy.TableName("Permission"))
}

func permissionIDColumn() string {
	return db.NamingStrategy.TableName("Permission") + ".id"
}

func CreatePermission(p *model.Permission) error {
	return errors.WithStack(db.Create(p).Error)
}

// DistinctPermissionActions returns every distinct action string that appears
// in a stored permission row. Actions are persisted as a JSON array per row, so
// the column cannot be de-duped in SQL; the catalog's orphan detection
// (op.ActionCatalog) collects the distinct set in memory. Rows whose actions
// JSON fails to parse are skipped rather than failing the whole catalog.
func DistinctPermissionActions() ([]model.Action, error) {
	var raw []string
	if err := db.Model(&model.Permission{}).Pluck("actions", &raw).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	seen := map[model.Action]struct{}{}
	var out []model.Action
	for _, s := range raw {
		if s == "" {
			continue
		}
		var acts model.Actions
		if err := json.Unmarshal([]byte(s), &acts); err != nil {
			continue
		}
		for _, a := range acts {
			if _, ok := seen[a]; !ok {
				seen[a] = struct{}{}
				out = append(out, a)
			}
		}
	}
	return out, nil
}

func UpdatePermission(p *model.Permission) error {
	return errors.WithStack(db.Save(p).Error)
}

// DeletePermissionById removes the permission and its role bindings atomically.
func DeletePermissionById(id uint) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("permission_id = ?", id).Delete(&model.RolePermission{}).Error; err != nil {
			return err
		}
		return tx.Delete(&model.Permission{}, id).Error
	}))
}

func GetPermissionById(id uint) (*model.Permission, error) {
	var p model.Permission
	if err := db.First(&p, id).Error; err != nil {
		return nil, errors.Wrapf(err, "failed get permission")
	}
	return &p, nil
}

func GetPermissions(pageIndex, pageSize int) (ps []model.Permission, count int64, err error) {
	q := db.Model(&model.Permission{})
	if err = q.Count(&count).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed count permissions")
	}
	if err = q.Order(columnName("id")).Offset((pageIndex - 1) * pageSize).Limit(pageSize).Find(&ps).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed find permissions")
	}
	return ps, count, nil
}

func GetPermissionsByRoleIDs(roleIDs []uint) ([]model.Permission, error) {
	if len(roleIDs) == 0 {
		return nil, nil
	}
	var ps []model.Permission
	err := db.Model(&model.Permission{}).
		Joins(rolePermissionsJoin()).
		Where("rp.role_id IN ?", roleIDs).
		Distinct().Find(&ps).Error
	return ps, errors.WithStack(err)
}

// GetManualPermissionsByNodeIDs returns the manual permission rows anchored on
// any of the given nodes — the node-based replacement for the anchor-prefix
// subtree query. The caller collects the subtree's node IDs (root + descendants
// via GetChildNodes) and passes them here; scope-origin rows are excluded (their
// truth is the role's declaration, not a node). Empty input is an empty result.
func GetManualPermissionsByNodeIDs(nodeIDs []uint) ([]model.Permission, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	var ps []model.Permission
	if err := db.Where("node_id IN ? AND origin = ?", nodeIDs, model.OriginManual).
		Find(&ps).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return ps, nil
}

// GetNodelessPermissions returns permission rows not yet anchored to a node
// (node_id 0): system-domain rows, scope declarations, above-mount virtual
// paths, and migration rows awaiting first-load adoption. Callers filter by
// origin/type (op.adoptNodelessAnchors adopts only manual fs rows).
func GetNodelessPermissions() ([]model.Permission, error) {
	var ps []model.Permission
	if err := db.Where("node_id = 0 OR node_id IS NULL").Find(&ps).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return ps, nil
}

// SetPermissionNodeID backfills a permission's node_id (first-load adoption).
func SetPermissionNodeID(id, nodeID uint) error {
	return errors.WithStack(db.Model(&model.Permission{}).Where("id = ?", id).
		Update("node_id", nodeID).Error)
}

// DeleteScopePermissionsByRole removes the role's scope-derived permissions
// and their bindings; manual permissions bound to the role are untouched.
func DeleteScopePermissionsByRole(roleID uint) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		var ids []uint
		if err := tx.Model(&model.Permission{}).
			Joins(rolePermissionsJoin()).
			Where("rp.role_id = ? AND origin = ?", roleID, model.OriginScope).
			Pluck(permissionIDColumn(), &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := tx.Where("permission_id IN ?", ids).Delete(&model.RolePermission{}).Error; err != nil {
			return err
		}
		return tx.Delete(&model.Permission{}, ids).Error
	}))
}

// DeletePermissionsByIDs removes the given permissions and their role bindings
// atomically. Empty input is a no-op. Used by the move/rename detach option to
// drop the manual permission rows the follow relocation carried into the moved
// subtree (spec 0-C): the caller captures the row IDs before the move and
// deletes them here only after the move succeeds, so a failed move deletes
// nothing.
func DeletePermissionsByIDs(ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("permission_id IN ?", ids).Delete(&model.RolePermission{}).Error; err != nil {
			return err
		}
		return tx.Delete(&model.Permission{}, ids).Error
	}))
}

// CloneManualPermission bundles a to-be-created permission row with the role
// IDs it must be bound to, so a clone reproduces both the row and every binding
// of its source in one transaction. An unbound permission row is inert (no role
// grants it), so the bindings are as load-bearing as the row itself.
type CloneManualPermission struct {
	Permission model.Permission
	RoleIDs    []uint
}

// ClonePermissionsWithBindings creates each permission and its role bindings in
// one transaction. Each Permission is created fresh (ID assigned by the DB) and
// then bound to its RoleIDs; a duplicate binding is ignored rather than failing
// the batch. Backs the copy_perms option (spec 0-C).
func ClonePermissionsWithBindings(clones []CloneManualPermission) error {
	if len(clones) == 0 {
		return nil
	}
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		for i := range clones {
			if err := tx.Create(&clones[i].Permission).Error; err != nil {
				return err
			}
			for _, rid := range clones[i].RoleIDs {
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
					Create(&model.RolePermission{RoleID: rid, PermissionID: clones[i].Permission.ID}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	}))
}
