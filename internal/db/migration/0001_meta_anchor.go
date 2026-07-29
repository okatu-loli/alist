package migration

import (
	"sort"
	"strings"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

func init() {
	register(&gormigrate.Migration{
		ID: "0001_meta_anchor",
		Migrate: func(tx *gorm.DB) error {
			// local frozen shape of the meta table at this migration:
			// baseline columns + the three anchor columns being added.
			type Meta struct {
				ID         uint   `gorm:"primaryKey"`
				Path       string `gorm:"unique"`
				Password   string
				PSub       bool
				Write      bool
				WSub       bool
				Hide       string
				HSub       bool
				Readme     string
				RSub       bool
				Header     string
				HeaderSub  bool
				StorageID  uint   `gorm:"index:idx_meta_anchor"`
				Anchor     string `gorm:"size:255;index:idx_meta_anchor"`
				AnchorKind uint8
			}
			if err := tx.AutoMigrate(&Meta{}); err != nil {
				return err
			}

			// Backfill from mount points only (pure DB data): longest
			// mount_path prefix wins. Metas above every mount point stay
			// path-anchored (zero storage_id, empty anchor) — legal state.
			var mounts []struct {
				ID        uint
				MountPath string
			}
			if err := tx.Model(&Storage{}).Select("id", "mount_path").Find(&mounts).Error; err != nil {
				return err
			}
			sort.Slice(mounts, func(i, j int) bool { return len(mounts[i].MountPath) > len(mounts[j].MountPath) })

			var metas []Meta
			if err := tx.Find(&metas).Error; err != nil {
				return err
			}
			for _, m := range metas {
				if m.Anchor != "" || m.StorageID != 0 {
					continue // idempotent re-run
				}
				for _, mt := range mounts {
					rel, ok := relUnder(mt.MountPath, m.Path)
					if !ok {
						continue
					}
					if err := tx.Model(&Meta{}).Where("id = ?", m.ID).Updates(map[string]interface{}{
						"storage_id": mt.ID, "anchor": rel, "anchor_kind": 0,
					}).Error; err != nil {
						return err
					}
					break
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			type Meta struct{}
			m := tx.Migrator()
			if m.HasIndex(&Meta{}, "idx_meta_anchor") {
				if err := m.DropIndex(&Meta{}, "idx_meta_anchor"); err != nil {
					return err
				}
			}
			for _, col := range []string{"storage_id", "anchor", "anchor_kind"} {
				if m.HasColumn(&Meta{}, col) {
					if err := m.DropColumn(&Meta{}, col); err != nil {
						return err
					}
				}
			}
			return nil
		},
	})
}

// relUnder returns path relative to mount ("/" for the mount root) and
// whether path is inside mount. Both are clean absolute paths.
func relUnder(mount, path string) (string, bool) {
	if mount == "/" {
		return path, true
	}
	if path == mount {
		return "/", true
	}
	if strings.HasPrefix(path, mount+"/") {
		return path[len(mount):], true
	}
	return "", false
}
