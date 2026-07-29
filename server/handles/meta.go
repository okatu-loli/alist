package handles

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/dlclark/regexp2"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// ListMetas returns a paginated list of path metadata rules.
// @Summary List metas
// @Description Returns a paginated list of path metadata rules (hide/password/readme/header settings per path).
// @Tags admin-meta
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param page query int false "page number"
// @Param per_page query int false "page size"
// @Success 200 {object} common.Resp[common.PageResp] "data.content is []model.Meta"
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/meta/list [get]
// @Router /v4/admin/metas [get]
func ListMetas(c *gin.Context) {
	var req model.PageReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Validate()
	log.Debugf("%+v", req)
	metas, total, err := op.GetMetas(req.Page, req.PerPage)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, common.PageResp{
		Content: metas,
		Total:   total,
	})
}

// CreateMeta creates a new path metadata rule.
// @Summary Create meta
// @Description Creates a new path metadata rule. The Hide field is validated as newline-separated regexp2 patterns.
// @Tags admin-meta
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body model.Meta true "meta to create"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid body or invalid Hide regexp"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/meta/create [post]
// @Router /v4/admin/metas [post]
func CreateMeta(c *gin.Context) {
	var req model.Meta
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	r, err := validHide(req.Hide)
	if err != nil {
		common.ErrorStrResp(c, fmt.Sprintf("%s is illegal: %s", r, err.Error()), 400)
		return
	}
	if err := op.CreateMeta(&req); err != nil {
		common.ErrorResp(c, err, 500, true)
	} else {
		common.SuccessResp(c)
	}
}

// UpdateMeta updates an existing path metadata rule.
// @Summary Update meta
// @Description Updates an existing path metadata rule (matched by ID in the body). The Hide field is validated as newline-separated regexp2 patterns.
// @Tags admin-meta
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body model.Meta true "meta to update, including id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid body or invalid Hide regexp"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/meta/update [post]
// @Param id path string true "meta id"
// @Router /v4/admin/metas/{id} [put]
func UpdateMeta(c *gin.Context) {
	var req model.Meta
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	r, err := validHide(req.Hide)
	if err != nil {
		common.ErrorStrResp(c, fmt.Sprintf("%s is illegal: %s", r, err.Error()), 400)
		return
	}
	if err := op.UpdateMeta(&req); err != nil {
		common.ErrorResp(c, err, 500, true)
	} else {
		common.SuccessResp(c)
	}
}

func validHide(hide string) (string, error) {
	rs := strings.Split(hide, "\n")
	for _, r := range rs {
		_, err := regexp2.Compile(r, regexp2.None)
		if err != nil {
			return r, err
		}
	}
	return "", nil
}

// DeleteMeta deletes a path metadata rule by ID.
// @Summary Delete meta
// @Description Deletes a path metadata rule by ID.
// @Tags admin-meta
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "meta id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/meta/delete [post]
// @Param id path string true "meta id"
// @Router /v4/admin/metas/{id} [delete]
func DeleteMeta(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.DeleteMetaById(uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}

// GetMeta returns a single path metadata rule by ID.
// @Summary Get meta
// @Description Returns a single path metadata rule by ID.
// @Tags admin-meta
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id query int true "meta id"
// @Success 200 {object} common.Resp[model.Meta]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/meta/get [get]
// @Param id path string true "meta id"
// @Router /v4/admin/metas/{id} [get]
func GetMeta(c *gin.Context) {
	idStr := pathOrQuery(c, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	meta, err := op.GetMetaById(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, meta)
}
