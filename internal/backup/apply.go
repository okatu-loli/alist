package backup

import (
	"archive/tar"
	"compress/gzip"
	"io"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// maxArchiveEntry caps a single decompressed tar member (defense against a
// zip-bomb-shaped archive). No exported domain is remotely this large.
const maxArchiveEntry = 256 << 20 // 256 MiB

func readTarGz(r io.Reader) (map[string][]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, errors.Wrap(err, "backup: opening gzip")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, "backup: reading tar")
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxArchiveEntry+1))
		if err != nil {
			return nil, errors.Wrapf(err, "backup: reading %s", hdr.Name)
		}
		if int64(len(data)) > maxArchiveEntry {
			return nil, errors.Errorf("backup: entry %s exceeds size limit", hdr.Name)
		}
		files[hdr.Name] = data
	}
	return files, nil
}

// applyToTx writes the archive into tx. Ordering (importDomainOrder) ensures
// each binding is resolved only after the rows it names exist. Ids are
// preserved on overwrite (tables are emptied first, so a backup restores as an
// id-faithful image — which the built-in roles, pinned to guest=1/admin=2 and
// hardcoded into IsAdmin/IsGuest, and the storage_id anchor references depend
// on). Under merge, matched rows are updated in place at their existing id and
// new rows take a fresh id; a storage id remap then keeps meta/permission
// anchors pointing at the right storage, and each affected user's Role column
// is rebuilt from the name-resolved bindings so authorization membership is
// correct regardless of how role ids landed.
func applyToTx(tx *gorm.DB, arc *Archive, strategy string, state *dbState) error {
	overwrite := strategy == StrategyOverwrite
	if overwrite {
		if err := deleteAll(tx); err != nil {
			return err
		}
	}

	if err := applySettings(tx, arc.Settings, overwrite, state); err != nil {
		return err
	}
	if err := applyStorages(tx, arc.Storages, overwrite, state); err != nil {
		return err
	}
	if err := applyMetas(tx, arc.Metas, overwrite, state); err != nil {
		return err
	}
	if err := applyRoles(tx, arc.Roles, overwrite, state); err != nil {
		return err
	}
	if err := applyPermissions(tx, arc.Permissions, overwrite, state); err != nil {
		return err
	}
	if err := applyUsers(tx, arc.Users, overwrite, state); err != nil {
		return err
	}

	// Rebuild name->id maps from the final (post-insert) state so bindings
	// resolve to the ids rows actually landed on.
	userIDs, roleIDs, permIDs, err := loadNameIDMaps(tx)
	if err != nil {
		return err
	}
	if err := applyUserRoles(tx, arc.UserRoles, userIDs, roleIDs); err != nil {
		return err
	}
	return applyRolePermissions(tx, arc.RolePermissions, roleIDs, permIDs)
}

// deleteAll empties every managed domain, bindings first so no row is orphaned
// mid-delete. AllowGlobalUpdate authorizes the condition-less deletes.
func deleteAll(tx *gorm.DB) error {
	g := tx.Session(&gorm.Session{AllowGlobalUpdate: true})
	for _, m := range []any{
		&model.RolePermission{}, &model.UserRole{},
		&model.Permission{}, &model.Role{}, &model.User{},
		&model.Meta{}, &model.Storage{}, &model.SettingItem{},
	} {
		if err := g.Delete(m).Error; err != nil {
			return errors.Wrap(err, "backup: clearing domain")
		}
	}
	return nil
}

func applySettings(tx *gorm.DB, items []model.SettingItem, overwrite bool, state *dbState) error {
	for i := range items {
		s := items[i]
		if _, exists := state.settingKeys[s.Key]; overwrite || !exists {
			if err := tx.Create(&s).Error; err != nil {
				return errors.Wrapf(err, "backup: inserting setting %s", s.Key)
			}
		} else {
			if err := tx.Save(&s).Error; err != nil {
				return errors.Wrapf(err, "backup: updating setting %s", s.Key)
			}
		}
	}
	return nil
}

func applyStorages(tx *gorm.DB, storages []model.Storage, overwrite bool, state *dbState) error {
	for i := range storages {
		s := storages[i]
		if dbID, exists := state.storageIDByMount[s.MountPath]; !overwrite && exists {
			s.ID = dbID
			if err := tx.Save(&s).Error; err != nil {
				return errors.Wrapf(err, "backup: updating storage %s", s.MountPath)
			}
		} else {
			if !overwrite {
				s.ID = 0 // let the db assign a free id to avoid colliding with an unrelated row
			}
			if err := tx.Create(&s).Error; err != nil {
				return errors.Wrapf(err, "backup: inserting storage %s", s.MountPath)
			}
		}
	}
	return nil
}

func applyMetas(tx *gorm.DB, metas []model.Meta, overwrite bool, state *dbState) error {
	for i := range metas {
		m := metas[i]
		if dbID, exists := state.metaIDByPath[m.Path]; !overwrite && exists {
			m.ID = dbID
			if err := tx.Save(&m).Error; err != nil {
				return errors.Wrapf(err, "backup: updating meta %s", m.Path)
			}
		} else {
			if !overwrite {
				m.ID = 0
			}
			if err := tx.Create(&m).Error; err != nil {
				return errors.Wrapf(err, "backup: inserting meta %s", m.Path)
			}
		}
	}
	return nil
}

func applyRoles(tx *gorm.DB, roles []model.Role, overwrite bool, state *dbState) error {
	for i := range roles {
		r := roles[i]
		if dbID, exists := state.roleIDByName[r.Name]; !overwrite && exists {
			r.ID = dbID
			if err := tx.Save(&r).Error; err != nil {
				return errors.Wrapf(err, "backup: updating role %s", r.Name)
			}
		} else {
			if !overwrite {
				r.ID = 0
			}
			if err := tx.Create(&r).Error; err != nil {
				return errors.Wrapf(err, "backup: inserting role %s", r.Name)
			}
		}
	}
	return nil
}

func applyPermissions(tx *gorm.DB, perms []model.Permission, overwrite bool, state *dbState) error {
	for i := range perms {
		p := perms[i]
		if dbID, exists := state.permIDByName[p.Name]; !overwrite && exists {
			p.ID = dbID
			if err := tx.Save(&p).Error; err != nil {
				return errors.Wrapf(err, "backup: updating permission %s", p.Name)
			}
		} else {
			if !overwrite {
				p.ID = 0
			}
			if err := tx.Create(&p).Error; err != nil {
				return errors.Wrapf(err, "backup: inserting permission %s", p.Name)
			}
		}
	}
	return nil
}

func applyUsers(tx *gorm.DB, users []userRecord, overwrite bool, state *dbState) error {
	for i := range users {
		u := users[i].toModel()
		if dbID, exists := state.userIDByName[u.Username]; !overwrite && exists {
			u.ID = dbID
			if err := tx.Save(&u).Error; err != nil {
				return errors.Wrapf(err, "backup: updating user %s", u.Username)
			}
		} else {
			if !overwrite {
				u.ID = 0
			}
			if err := tx.Create(&u).Error; err != nil {
				return errors.Wrapf(err, "backup: inserting user %s", u.Username)
			}
		}
	}
	return nil
}

// applyUserRoles inserts the name-resolved user_roles rows (idempotently) and
// then rewrites each touched user's Role column to the full set of role ids in
// user_roles for that user — the column the policy engine reads, kept exact
// regardless of id shifts.
func applyUserRoles(tx *gorm.DB, bindings []userRoleBinding, userIDs, roleIDs map[string]uint) error {
	affected := map[uint]struct{}{}
	for _, b := range bindings {
		uid, rid := userIDs[b.Username], roleIDs[b.RoleName]
		if uid == 0 || rid == 0 {
			return errors.Errorf("backup: user_role binding %s/%s references a missing row", b.Username, b.RoleName)
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&model.UserRole{UserID: uid, RoleID: rid}).Error; err != nil {
			return errors.Wrap(err, "backup: inserting user_role")
		}
		affected[uid] = struct{}{}
	}
	for uid := range affected {
		var rids []uint
		if err := tx.Model(&model.UserRole{}).Where("user_id = ?", uid).
			Pluck("role_id", &rids).Error; err != nil {
			return errors.Wrap(err, "backup: reading user roles for rebuild")
		}
		roles := make(model.Roles, len(rids))
		for i, r := range rids {
			roles[i] = int(r)
		}
		if err := tx.Model(&model.User{}).Where("id = ?", uid).
			Update("role", roles).Error; err != nil {
			return errors.Wrap(err, "backup: rebuilding user role column")
		}
	}
	return nil
}

func applyRolePermissions(tx *gorm.DB, bindings []rolePermissionBinding, roleIDs, permIDs map[string]uint) error {
	for _, b := range bindings {
		rid, pid := roleIDs[b.RoleName], permIDs[b.PermissionName]
		if rid == 0 || pid == 0 {
			return errors.Errorf("backup: role_permission binding %s/%s references a missing row", b.RoleName, b.PermissionName)
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&model.RolePermission{RoleID: rid, PermissionID: pid}).Error; err != nil {
			return errors.Wrap(err, "backup: inserting role_permission")
		}
	}
	return nil
}

// loadNameIDMaps reads the post-insert name->id maps used to resolve bindings.
// Permission names are treated as unique natural keys (the scope-derived rows
// are named role:<role>:scope:<n>, manual rows are admin-named); a duplicate
// name resolves to whichever row is read last.
func loadNameIDMaps(tx *gorm.DB) (users, roles, perms map[string]uint, err error) {
	users, roles, perms = map[string]uint{}, map[string]uint{}, map[string]uint{}
	var us []model.User
	if err = tx.Find(&us).Error; err != nil {
		return nil, nil, nil, errors.Wrap(err, "backup: reloading users")
	}
	for _, u := range us {
		users[u.Username] = u.ID
	}
	var rs []model.Role
	if err = tx.Find(&rs).Error; err != nil {
		return nil, nil, nil, errors.Wrap(err, "backup: reloading roles")
	}
	for _, r := range rs {
		roles[r.Name] = r.ID
	}
	var ps []model.Permission
	if err = tx.Find(&ps).Error; err != nil {
		return nil, nil, nil, errors.Wrap(err, "backup: reloading permissions")
	}
	for _, p := range ps {
		perms[p.Name] = p.ID
	}
	return users, roles, perms, nil
}
