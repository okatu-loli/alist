package handles

import (
	"context"
	"strconv"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// ListStorages returns a paginated list of configured storages.
// @Summary List storages
// @Description Returns a paginated list of all configured storages.
// @Tags admin-storage
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param page query int false "page number (default 1)"
// @Param per_page query int false "items per page (default: unlimited)"
// @Success 200 {object} common.Resp[common.PageResp] "Content: []model.Storage"
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/storage/list [get]
// @Router /v4/admin/storages [get]
func ListStorages(c *gin.Context) {
	var req model.PageReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Validate()
	log.Debugf("%+v", req)
	storages, total, err := db.GetStorages(req.Page, req.PerPage)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	// G3 §1: never echo a stored secret Additional field (password, API
	// token, ...) back in full — see op.RedactStorageAddition.
	for i := range storages {
		storages[i] = op.RedactStorageAddition(storages[i])
	}
	common.SuccessResp(c, common.PageResp{
		Content: storages,
		Total:   total,
	})
}

// CreateStorage creates a new storage.
// @Summary Create storage
// @Description Creates a new storage and (best-effort) loads its driver.
// @Tags admin-storage
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body model.Storage true "storage config"
// @Success 200 {object} common.Resp[gin.H] "data: {id}"
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[gin.H] "data: {id}; storage was created but driver failed to load"
// @Router /admin/storage/create [post]
// @Router /v4/admin/storages [post]
func CreateStorage(c *gin.Context) {
	var req model.Storage
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if id, err := op.CreateStorage(c, req); err != nil {
		common.ErrorWithDataResp(c, err, 500, gin.H{
			"id": id,
		}, true)
	} else {
		common.SuccessResp(c, gin.H{
			"id": id,
		})
	}
}

// UpdateStorage updates an existing storage.
// @Summary Update storage
// @Description Updates an existing storage's config and reloads its driver.
// @Tags admin-storage
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body model.Storage true "storage config (must include id)"
// @Param id path string true "storage id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/storage/update [post]
// @Router /v4/admin/storages/{id} [put]
func UpdateStorage(c *gin.Context) {
	var req model.Storage
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.UpdateStorage(c, req); err != nil {
		common.ErrorResp(c, err, 500, true)
	} else {
		common.SuccessResp(c)
	}
}

// DeleteStorage deletes a storage by id.
// @Summary Delete storage
// @Description Deletes a storage by id and drops its driver.
// @Tags admin-storage
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "storage id"
// @Param id path string true "storage id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/storage/delete [post]
// @Router /v4/admin/storages/{id} [delete]
func DeleteStorage(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.DeleteStorageById(c, uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}

// DisableStorage disables a storage by id.
// @Summary Disable storage
// @Description Disables a storage by id and drops its driver.
// @Tags admin-storage
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "storage id"
// @Param id path string true "storage id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/storage/disable [post]
// @Router /v4/admin/storages/{id}/disable [post]
func DisableStorage(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.DisableStorage(c, uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}

// EnableStorage enables a storage by id.
// @Summary Enable storage
// @Description Enables a storage by id and loads its driver.
// @Tags admin-storage
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "storage id"
// @Param id path string true "storage id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/storage/enable [post]
// @Router /v4/admin/storages/{id}/enable [post]
func EnableStorage(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.EnableStorage(c, uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}

// GetStorage returns a single storage by id.
// @Summary Get storage
// @Description Returns a single storage's config by id.
// @Tags admin-storage
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "storage id"
// @Param id path string true "storage id"
// @Success 200 {object} common.Resp[model.Storage]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/storage/get [get]
// @Router /v4/admin/storages/{id} [get]
func GetStorage(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	storage, err := db.GetStorageById(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	// G3 §1: never echo a stored secret Additional field back in full.
	common.SuccessResp(c, op.RedactStorageAddition(*storage))
}

// LoadAllStorages asynchronously drops and reloads every enabled storage's driver.
// @Summary Reload all storages
// @Description Asynchronously drops and reloads the driver for every enabled storage.
// @Tags admin-storage
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/storage/load_all [post]
// @Router /v4/admin/storages/load_all [post]
func LoadAllStorages(c *gin.Context) {
	storages, err := db.GetEnabledStorages()
	if err != nil {
		log.Errorf("failed get enabled storages: %+v", err)
		common.ErrorResp(c, err, 500, true)
		return
	}
	conf.StoragesLoaded = false
	go func(storages []model.Storage) {
		for _, storage := range storages {
			storageDriver, err := op.GetStorageByMountPath(storage.MountPath)
			if err != nil {
				log.Errorf("failed get storage driver: %+v", err)
				continue
			}
			// drop the storage in the driver
			if err := storageDriver.Drop(context.Background()); err != nil {
				log.Errorf("failed drop storage: %+v", err)
				continue
			}
			if err := op.LoadStorage(context.Background(), storage); err != nil {
				log.Errorf("failed get enabled storages: %+v", err)
				continue
			}
			log.Infof("success load storage: [%s], driver: [%s]",
				storage.MountPath, storage.Driver)
		}
		conf.StoragesLoaded = true
	}(storages)
	common.SuccessResp(c)
}
