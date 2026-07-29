package handles

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/sign"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// ListSigningKeys returns every key (active and grace) for a purpose. The
// secret never leaves internal/model.SigningKey.Secret (json:"-"), so it's
// safe to serialize the rows straight through.
// @Summary List signing keys
// @Description Returns every key (active and grace) for a purpose. The secret is never serialized.
// @Tags admin-signing-key
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param purpose query string false "signing key purpose (default: download)"
// @Success 200 {object} common.Resp[common.PageResp] "Content: []model.SigningKey"
// @Failure 500 {object} common.Resp[any]
// @Router /admin/signing_key/list [get]
// @Router /v4/admin/signing_keys [get]
func ListSigningKeys(c *gin.Context) {
	purpose := c.Query("purpose")
	if purpose == "" {
		purpose = model.SignPurposeDownload
	}
	keys, err := sign.ListKeys(purpose)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, common.PageResp{Content: keys, Total: int64(len(keys))})
}

type SigningKeyPurposeReq struct {
	Purpose string `json:"purpose" form:"purpose" binding:"required"`
}

// RotateSigningKey deactivates the current active key for the purpose and
// installs a new active one; the old key keeps verifying until GCSigningKeys
// removes it.
// @Summary Rotate signing key
// @Description Deactivates the current active key for the purpose and installs a new active one; the old key keeps verifying until GCSigningKeys removes it.
// @Tags admin-signing-key
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body SigningKeyPurposeReq true "signing key purpose"
// @Success 200 {object} common.Resp[gin.H] "data: {kid}"
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/signing_key/rotate [post]
// @Router /v4/admin/signing_keys/rotate [post]
func RotateSigningKey(c *gin.Context) {
	var req SigningKeyPurposeReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	kid, err := sign.Rotate(req.Purpose)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, gin.H{"kid": kid})
}

// GCSigningKeys deletes every non-active key for the purpose, ending their
// grace window.
// @Summary GC signing keys
// @Description Deletes every non-active key for the purpose, ending their grace window.
// @Tags admin-signing-key
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body SigningKeyPurposeReq true "signing key purpose"
// @Success 200 {object} common.Resp[gin.H] "data: {removed}"
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /admin/signing_key/gc [post]
// @Router /v4/admin/signing_keys/gc [post]
func GCSigningKeys(c *gin.Context) {
	var req SigningKeyPurposeReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	removed, err := sign.GC(req.Purpose)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, gin.H{"removed": removed})
}
