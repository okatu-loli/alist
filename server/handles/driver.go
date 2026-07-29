package handles

import (
	"fmt"

	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// ListDriverInfo returns the config schema (items) for every registered driver, keyed by driver name.
// Plugin-backed drivers' Additional item Label/Help are localized to the
// request's negotiated language (Accept-Language, via i18n.Lang) from that
// plugin's bundled i18n/ locale files (Task ②); built-in drivers are
// host-generated in English and unaffected by locale.
// @Summary List driver info
// @Description Returns the config item schema for every registered storage driver, keyed by driver name.
// @Tags admin-driver
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[any] "data: map[string][]driver.Item"
// @Router /admin/driver/list [get]
// @Router /v4/admin/drivers [get]
func ListDriverInfo(c *gin.Context) {
	common.SuccessResp(c, op.GetDriverInfoMapLocale(i18n.Lang(c)))
}

// ListDriverNames returns the names of all registered drivers.
// @Summary List driver names
// @Description Returns the names of all registered storage drivers.
// @Tags admin-driver
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[[]string]
// @Router /admin/driver/names [get]
// @Router /v4/admin/drivers/names [get]
func ListDriverNames(c *gin.Context) {
	common.SuccessResp(c, op.GetDriverNames())
}

// GetDriverInfo returns the config schema (items) for a single driver.
// @Summary Get driver info
// @Description Returns the config item schema for a single driver by name.
// @Tags admin-driver
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param driver query string true "driver name"
// @Param driver path string true "driver name"
// @Success 200 {object} common.Resp[any] "data: []driver.Item"
// @Failure 404 {object} common.Resp[any] "driver not found"
// @Router /admin/driver/info [get]
// @Router /v4/admin/drivers/{driver} [get]
func GetDriverInfo(c *gin.Context) {
	driverName := pathOrQuery(c, "driver")
	infoMap := op.GetDriverInfoMapLocale(i18n.Lang(c))
	items, ok := infoMap[driverName]
	if !ok {
		common.ErrorStrResp(c, fmt.Sprintf("driver [%s] not found", driverName), 404)
		return
	}
	common.SuccessResp(c, items)
}
