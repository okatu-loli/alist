package handles

import (
	"context"

	ext "github.com/AlistGo/alist-ext"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/search"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/server/common"
)

// Index management endpoints (子项目 A, wave A3): admin-only Build/Stop/Clear/
// Progress over the search index. The build runs in a detached goroutine so the
// request returns immediately; progress is polled via GetProgress. UpdateIndex
// (incremental) is deferred to the auto-update wave (A7).

// BuildIndex kicks off a full rebuild: it clears the current index, then walks
// every storage as admin feeding the active backend. Rejected (400) while a build
// is already running. The heavy work is detached so the HTTP call returns at once.
// @Summary Build search index
// @Description Clears the current index, then walks every storage as admin feeding it into the active search backend. The heavy work runs detached, so the call returns immediately. Rejected while a build is already running.
// @Tags admin-search-index
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "index is already running"
// @Router /v4/admin/index/build [post]
func BuildIndex(c *gin.Context) {
	if search.Running() {
		common.ErrorStrResp(c, "index is running", 400)
		return
	}
	maxDepth := setting.GetInt(conf.MaxIndexDepth, 20)
	ignorePaths := search.ParseIgnorePaths(setting.GetStr(conf.IgnorePaths))
	go func() {
		ctx := context.Background()
		if err := search.Clear(ctx); err != nil {
			log.Errorf("clear index error: %+v", err)
			return
		}
		if err := search.BuildIndex(ctx, []string{"/"}, ignorePaths, maxDepth, true); err != nil {
			log.Errorf("build index error: %+v", err)
		}
	}()
	common.SuccessResp(c)
}

// UpdateIndexReq targets an incremental (re)index of specific paths. Paths
// defaults to the whole tree ("/") when empty; MaxDepth defaults to the
// max_index_depth setting when unset.
type UpdateIndexReq struct {
	Paths    []string `json:"paths"`
	MaxDepth int      `json:"max_depth"`
}

// UpdateIndex targeted-rebuilds the given paths against the active backend: it
// drops each path's subtree from the index, then re-walks it. Rejected (400)
// while a build is running, or when the active backend does not support
// incremental auto-update. The heavy work is detached so the call returns at once.
// @Summary Update search index
// @Description Incrementally re-indexes the given paths (defaults to the whole tree) against the active backend, dropping and re-walking each path's subtree. Runs detached; rejected while a build is running or when the backend does not support incremental update.
// @Tags admin-search-index
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body UpdateIndexReq false "paths + max depth, both optional"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "index is running, or update not supported"
// @Router /v4/admin/index/update [post]
func UpdateIndex(c *gin.Context) {
	var req UpdateIndexReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if search.Running() {
		common.ErrorStrResp(c, "index is running", 400)
		return
	}
	if !search.Config().AutoUpdate {
		common.ErrorStrResp(c, "update is not supported for current index", 400)
		return
	}
	paths := req.Paths
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	maxDepth := req.MaxDepth
	if maxDepth <= 0 {
		maxDepth = setting.GetInt(conf.MaxIndexDepth, 20)
	}
	ignorePaths := search.ParseIgnorePaths(setting.GetStr(conf.IgnorePaths))
	go func() {
		ctx := context.Background()
		for _, p := range paths {
			if err := search.Del(ctx, p); err != nil {
				log.Errorf("update index: del %s: %+v", p, err)
				return
			}
		}
		if err := search.BuildIndex(ctx, paths, ignorePaths, maxDepth, false); err != nil {
			log.Errorf("update index error: %+v", err)
		}
	}()
	common.SuccessResp(c)
}

// StopIndex asks an in-flight build to abort. Rejected (400) when nothing is
// running.
// @Summary Stop search index build
// @Description Asks an in-flight index build to abort. Rejected when nothing is running.
// @Tags admin-search-index
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "index is not running"
// @Router /v4/admin/index/stop [post]
func StopIndex(c *gin.Context) {
	if !search.Running() {
		common.ErrorStrResp(c, "index is not running", 400)
		return
	}
	search.StopIndex()
	common.SuccessResp(c)
}

// ClearIndex drops the whole index and resets the persisted progress snapshot.
// Rejected (400) while a build is running (clearing under a live build would race
// the writer).
// @Summary Clear search index
// @Description Drops the whole search index and resets the persisted progress snapshot. Rejected while a build is running.
// @Tags admin-search-index
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "index is running"
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/index/clear [post]
func ClearIndex(c *gin.Context) {
	if search.Running() {
		common.ErrorStrResp(c, "index is running", 400)
		return
	}
	if err := search.Clear(c); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if err := search.WriteProgress(&ext.IndexProgress{
		ObjCount:     0,
		IsDone:       true,
		LastDoneTime: nil,
		Error:        "",
	}); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// GetProgress returns the persisted ext.IndexProgress snapshot for the frontend
// to poll during a build.
// @Summary Get search index progress
// @Description Returns the persisted index build progress snapshot for the caller to poll during a build.
// @Tags admin-search-index
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[ext.IndexProgress]
// @Failure 500 {object} common.Resp[any]
// @Router /v4/admin/index/progress [get]
func GetProgress(c *gin.Context) {
	progress, err := search.Progress()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, progress)
}
