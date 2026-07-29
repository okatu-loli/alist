package handles

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/backup"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

type CreateBackupReq struct {
	Password          string `json:"password"`            // when set, domain payloads are AES-256-GCM encrypted
	Sanitize          bool   `json:"sanitize"`            // strip storage credentials / user secrets (mutually exclusive with password)
	IncludeSecretsKey bool   `json:"include_secrets_key"` // carry the shared secrets key (storage/plugin secret encryption) so it can decrypt on restore to a different machine; requires password
}

type CreateBackupResp struct {
	Name     string          `json:"name"`
	Manifest backup.Manifest `json:"manifest"`
}

// CreateBackup writes a new backup archive to the server's backup directory.
// @Summary Create backup
// @Description Exports settings, users, roles, permissions, metas, storages and their bindings into a tar.gz under the data dir. Optionally password-encrypts the payloads, or sanitizes secrets for a shareable export. Admin only.
// @Tags admin_backup
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body CreateBackupReq false "backup options"
// @Success 200 {object} common.Resp[CreateBackupResp]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/backup [post]
func CreateBackup(c *gin.Context) {
	var req CreateBackupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	name, manifest, err := backup.Export(backup.Options{Password: req.Password, Sanitize: req.Sanitize, IncludeSecretsKey: req.IncludeSecretsKey})
	if err != nil {
		audit.Event(c, user.ID, user.Username, "backup.create", "backup:", false)
		common.ErrorResp(c, err, 500, true)
		return
	}
	audit.Event(c, user.ID, user.Username, "backup.create", "backup:"+name, true)
	common.SuccessResp(c, CreateBackupResp{Name: name, Manifest: manifest})
}

type BackupFile struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// ListBackups lists the backup archives on disk, newest first.
// @Summary List backups
// @Description Lists backup archives in the server's backup directory (name, size, mtime), newest first. Admin only.
// @Tags admin_backup
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[[]BackupFile]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/backup [get]
func ListBackups(c *gin.Context) {
	entries, err := os.ReadDir(backup.Dir())
	if err != nil {
		if os.IsNotExist(err) {
			common.SuccessResp(c, []BackupFile{})
			return
		}
		common.ErrorResp(c, err, 500, true)
		return
	}
	files := make([]BackupFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, BackupFile{Name: e.Name(), Size: info.Size(), CreatedAt: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].CreatedAt.After(files[j].CreatedAt) })
	common.SuccessResp(c, files)
}

// DownloadBackup streams a backup archive.
// @Summary Download backup
// @Description Streams the named backup archive (tar.gz). Admin only.
// @Tags admin_backup
// @Security ApiKeyAuth
// @Produce application/gzip
// @Param name path string true "backup file name"
// @Success 200 {file} binary
// @Failure 400 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/backup/{name}/download [get]
func DownloadBackup(c *gin.Context) {
	name := c.Param("name")
	if err := backup.ValidateName(name); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	path := filepath.Join(backup.Dir(), name)
	if _, err := os.Stat(path); err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	c.Header("Content-Disposition", "attachment; filename=\""+name+"\"")
	c.Header("Content-Type", "application/gzip")
	c.File(path)
}

// DeleteBackup removes a backup archive.
// @Summary Delete backup
// @Description Deletes the named backup archive. Admin only.
// @Tags admin_backup
// @Security ApiKeyAuth
// @Produce json
// @Param name path string true "backup file name"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/backup/{name} [delete]
func DeleteBackup(c *gin.Context) {
	name := c.Param("name")
	if err := backup.ValidateName(name); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	err := os.Remove(filepath.Join(backup.Dir(), name))
	if err != nil && !os.IsNotExist(err) {
		audit.Event(c, user.ID, user.Username, "backup.delete", "backup:"+name, false)
		common.ErrorResp(c, err, 500, true)
		return
	}
	audit.Event(c, user.ID, user.Username, "backup.delete", "backup:"+name, true)
	common.SuccessResp(c)
}

type RestoreBackupReq struct {
	Name     string `json:"name" binding:"required"`
	Password string `json:"password"`
	Strategy string `json:"strategy"` // "merge" | "overwrite"
	DryRun   bool   `json:"dry_run"`
}

// RestoreBackup restores a backup, or previews the change when dry_run is set.
// @Summary Restore backup
// @Description Restores the named backup with merge or overwrite semantics, or previews the diff when dry_run is true. A non-dry-run restore first snapshots current state (pre-restore-<ts>.tar.gz) and applies everything in one transaction. Restart the server afterwards so in-memory caches and storage drivers reload from the restored database. Admin only.
// @Tags admin_backup
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body RestoreBackupReq true "restore options"
// @Success 200 {object} common.Resp[backup.RestoreDiff]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/backup/restore [post]
func RestoreBackup(c *gin.Context) {
	var req RestoreBackupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if req.Strategy == "" {
		req.Strategy = backup.StrategyMerge
	}
	if req.Strategy != backup.StrategyMerge && req.Strategy != backup.StrategyOverwrite {
		common.ErrorStrResp(c, "strategy must be \"merge\" or \"overwrite\"", 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	diff, err := backup.Restore(backup.RestoreOptions{
		Name:     req.Name,
		Password: req.Password,
		Strategy: req.Strategy,
		DryRun:   req.DryRun,
	})
	if err != nil {
		// Restore failures are client-actionable inputs — bad name, schema
		// mismatch, wrong password, unknown strategy — so they report 400, with
		// a missing archive distinguished as 404.
		code := 400
		if errors.Is(err, os.ErrNotExist) {
			code = 404
		}
		if !req.DryRun {
			audit.Event(c, user.ID, user.Username, "backup.restore", "backup:"+req.Name, false)
		}
		common.ErrorResp(c, err, code)
		return
	}
	// A dry-run changes nothing, so it is not an audit-worthy security event;
	// only a real restore is recorded.
	if !req.DryRun {
		audit.Event(c, user.ID, user.Username, "backup.restore", "backup:"+req.Name, true)
	}
	// The returned diff carries the caller's dry_run intent so a client can tell
	// a preview from an applied restore.
	diff.DryRun = req.DryRun
	common.SuccessResp(c, diff)
}
