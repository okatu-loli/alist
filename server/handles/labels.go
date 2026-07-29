package handles

import (
	"strconv"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// Labels API (design §4). The vocabulary CRUD is admin-only (router:
// /admin/labels behind AuthAdmin); apply/remove require ActWrite on the target
// path and reading a path's labels requires ActRead — labels are shared file
// metadata, so changing them needs write permission and reading/filtering never
// leaks a path the caller cannot see. name/color are validated in the op layer
// (plain-text name, #RRGGBB color) so a stored value carries no injection risk.

// LabelReq is the vocabulary create/update body. On update the id is injected
// from the path param (ParamIntoJSONBody).
type LabelReq struct {
	ID    uint   `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// LabelBindingReq is the apply/remove body: which label, which virtual path.
type LabelBindingReq struct {
	Path    string `json:"path" form:"path"`
	LabelID uint   `json:"label_id" form:"label_id"`
}

// LabelFilesResp is the "files with this label" filter view: the caller-visible
// virtual paths a label is bound to.
type LabelFilesResp struct {
	Paths []string `json:"paths"`
}

// LabelsList returns the whole shared vocabulary (any logged-in user).
// @Summary List labels
// @Description Returns the entire shared label vocabulary. Any logged-in user may read it.
// @Tags labels
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[[]model.Label]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/labels [get]
func LabelsList(c *gin.Context) {
	labels, err := op.ListLabels()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, labels)
}

// LabelCreate adds a vocabulary entry (admin-only via router). name/color are
// validated in op.
// @Summary Create label
// @Description Adds a vocabulary entry (name/color) to the shared label set. Admin only.
// @Tags admin-labels
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body LabelReq true "label name/color"
// @Success 200 {object} common.Resp[model.Label]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/labels [post]
func LabelCreate(c *gin.Context) {
	var req LabelReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	l := &model.Label{Name: req.Name, Color: req.Color}
	if err := op.CreateLabel(l); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, l)
}

// LabelUpdate edits a vocabulary entry (admin-only via router). The id comes from
// the path param.
// @Summary Update label
// @Description Edits a vocabulary entry's name/color. Admin only. The id is injected from the path param into the JSON body.
// @Tags admin-labels
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path string true "label id"
// @Param body body LabelReq true "label name/color"
// @Success 200 {object} common.Resp[model.Label]
// @Failure 400 {object} common.Resp[any]
// @Router /v4/admin/labels/{id} [put]
func LabelUpdate(c *gin.Context) {
	var req LabelReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	l := &model.Label{ID: req.ID, Name: req.Name, Color: req.Color}
	if err := op.UpdateLabel(l); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, l)
}

// LabelDelete removes a vocabulary entry and cascades its bindings (admin-only
// via router).
// @Summary Delete label
// @Description Removes a vocabulary entry and cascades the removal to every file binding it. Admin only.
// @Tags admin-labels
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "label id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/labels/{id} [delete]
func LabelDelete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.DeleteLabel(uint(id)); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// LabelsOf returns the labels attached to a path. Requires ActRead on the path —
// you may only see the labels of files you may read.
// @Summary List a path's labels
// @Description Returns the labels bound to the object at path. Requires ActRead on the path — you may only see the labels of files you may read.
// @Tags labels
// @Security ApiKeyAuth
// @Produce json
// @Param path query string true "file path"
// @Success 200 {object} common.Resp[[]model.Label]
// @Failure 403 {object} common.Resp[any] "no read permission on path"
// @Failure 500 {object} common.Resp[any]
// @Router /v4/labels/of [get]
func LabelsOf(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(c.Query("path"))
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: reqPath}); err != nil {
		common.AuthzErrorResp(c, err, "you have no permission to view this path")
		return
	}
	labels, err := op.LabelsOf(reqPath)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, labels)
}

// LabelApply binds a label onto the object at a path. A label is shared file
// metadata, so applying it requires ActWrite on the path — no write permission,
// no relabel (403).
// @Summary Apply label
// @Description Binds a label onto the object at path. Requires ActWrite on the path — no write permission, no relabel.
// @Tags labels
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body LabelBindingReq true "path + label id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "no write permission on path"
// @Failure 404 {object} common.Resp[any] "label not found"
// @Failure 500 {object} common.Resp[any]
// @Router /v4/labels/apply [post]
func LabelApply(c *gin.Context) {
	var req LabelBindingReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActWrite, op.AuthzRequest{Path: reqPath}); err != nil {
		common.AuthzErrorResp(c, err, "you have no permission to modify labels on this path")
		return
	}
	// Reject a dangling binding to a non-existent label (404) before anchoring a
	// node for it.
	if _, err := op.GetLabel(req.LabelID); err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	if err := op.ApplyLabel(c, reqPath, req.LabelID); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// LabelRemove unbinds a label from the object at a path. Same ActWrite gate as
// apply: relabeling is a write to shared file metadata.
// @Summary Remove label
// @Description Unbinds a label from the object at path. Requires ActWrite on the path, same gate as apply.
// @Tags labels
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body LabelBindingReq true "path + label id"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "no write permission on path"
// @Failure 500 {object} common.Resp[any]
// @Router /v4/labels/remove [post]
func LabelRemove(c *gin.Context) {
	var req LabelBindingReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActWrite, op.AuthzRequest{Path: reqPath}); err != nil {
		common.AuthzErrorResp(c, err, "you have no permission to modify labels on this path")
		return
	}
	if err := op.RemoveLabel(c, reqPath, req.LabelID); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// LabelFiles lists the files a label is bound to — the "filter by label" view.
// Each bound node is resolved to its virtual path and authorized (ActRead) for
// the caller, so the view never reveals a path the caller may not see.
// @Summary List files with a label
// @Description Lists the caller-visible virtual paths bound to a label. Each bound node is authorized (ActRead) individually, so the view never reveals a path the caller may not see.
// @Tags labels
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "label id"
// @Success 200 {object} common.Resp[LabelFilesResp]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/labels/{id}/files [get]
func LabelFiles(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	nodeIDs, err := db.BindingsByLabel(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	paths := make([]string, 0, len(nodeIDs))
	for _, nid := range nodeIDs {
		vp, err := op.NodeVirtualPath(nid)
		if err != nil {
			continue // node's storage gone / unresolvable — skip
		}
		if op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: vp}) != nil {
			continue // drop paths the caller may not see
		}
		paths = append(paths, vp)
	}
	common.SuccessResp(c, LabelFilesResp{Paths: paths})
}
