package handles

import (
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/offline_download"
	"github.com/alist-org/alist/v3/internal/task"
	"math"
	"time"

	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	"github.com/xhofe/tache"
)

type TaskInfo struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Creator     string      `json:"creator"`
	CreatorRole model.Roles `json:"creator_role"`
	State       tache.State `json:"state"`
	Status      string      `json:"status"`
	Progress    float64     `json:"progress"`
	StartTime   *time.Time  `json:"start_time"`
	EndTime     *time.Time  `json:"end_time"`
	TotalBytes  int64       `json:"total_bytes"`
	Error       string      `json:"error"`
}

// getTaskInfo serializes a task for the API, rendering its name/status in the
// viewer's negotiated locale (c) when the task exposes keyed i18n accessors;
// otherwise it falls back to the task's own (English) GetName/GetStatus.
func getTaskInfo[T task.TaskExtensionInfo](c *gin.Context, t T) TaskInfo {
	errMsg := ""
	if t.GetErr() != nil {
		errMsg = t.GetErr().Error()
	}
	progress := t.GetProgress()
	// if progress is NaN, set it to 100
	if math.IsNaN(progress) {
		progress = 100
	}
	creatorName := ""
	var creatorRole model.Roles
	if t.GetCreator() != nil {
		creatorName = t.GetCreator().Username
		creatorRole = t.GetCreator().Role
	}
	name := t.GetName()
	status := t.GetStatus()
	if it, ok := any(t).(task.TaskInfoWithI18n); ok {
		lang := i18n.Lang(c)
		if key := it.GetNameKey(); key != "" {
			name = i18n.T(lang, key, it.GetNameArgs()...)
		}
		if key := it.GetStatusKey(); key != "" {
			status = i18n.T(lang, key, it.GetStatusArgs()...)
		}
	}
	return TaskInfo{
		ID:          t.GetID(),
		Name:        name,
		Creator:     creatorName,
		CreatorRole: creatorRole,
		State:       t.GetState(),
		Status:      status,
		Progress:    progress,
		StartTime:   t.GetStartTime(),
		EndTime:     t.GetEndTime(),
		TotalBytes:  t.GetTotalBytes(),
		Error:       errMsg,
	}
}

func getTaskInfos[T task.TaskExtensionInfo](c *gin.Context, tasks []T) []TaskInfo {
	return utils.MustSliceConvert(tasks, func(t T) TaskInfo {
		return getTaskInfo(c, t)
	})
}

func argsContains[T comparable](v T, slice ...T) bool {
	return utils.SliceContains(slice, v)
}

func getUserInfo(c *gin.Context) (bool, uint, bool) {
	if user, ok := c.Value(ctxkey.User).(*model.User); ok {
		return user.IsAdmin(), user.ID, true
	} else {
		return false, 0, false
	}
}

func getTargetedHandler[T task.TaskExtensionInfo](manager task.Manager[T], callback func(c *gin.Context, task T)) gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, uid, ok := getUserInfo(c)
		if !ok {
			// if there is no bug, here is unreachable
			common.ErrorI18n(c, "task.user_invalid", 401)
			return
		}
		t, ok := manager.GetByID(c.Query("tid"))
		if !ok {
			common.ErrorI18n(c, "task.not_found", 404)
			return
		}
		if !isAdmin && uid != t.GetCreator().ID {
			// to avoid an attacker using error messages to guess valid TID, return a 404 rather than a 403
			common.ErrorI18n(c, "task.not_found", 404)
			return
		}
		callback(c, t)
	}
}

func getBatchHandler[T task.TaskExtensionInfo](manager task.Manager[T], callback func(task T)) gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, uid, ok := getUserInfo(c)
		if !ok {
			common.ErrorI18n(c, "task.user_invalid", 401)
			return
		}
		var tids []string
		if err := c.ShouldBind(&tids); err != nil {
			common.ErrorI18n(c, "task.invalid_request_format", 400)
			return
		}
		retErrs := make(map[string]string)
		for _, tid := range tids {
			t, ok := manager.GetByID(tid)
			if !ok || (!isAdmin && uid != t.GetCreator().ID) {
				retErrs[tid] = i18n.T(i18n.Lang(c), "task.not_found")
				continue
			}
			callback(t)
		}
		common.SuccessResp(c, retErrs)
	}
}

// taskRoute registers the upload/copy task endpoints shared by the /api/task
// and /api/admin/task mounts. Each handler is a small named generic wrapper
// so swag can attach doc comments to it; the bodies are unchanged from their
// former inline-closure form. Every operation is documented once (see the
// doc comments below) with @Router lines for all four mount points
// (task/upload/*, task/copy/*, admin/task/upload/*, admin/task/copy/*), since
// the upload and copy task managers expose an identical HTTP contract.
func taskRoute[T task.TaskExtensionInfo](g *gin.RouterGroup, manager task.Manager[T]) {
	g.GET("/undone", taskUndoneHandler(manager))
	g.GET("/done", taskDoneHandler(manager))
	g.POST("/info", taskInfoHandler(manager))
	g.POST("/cancel", taskCancelHandler(manager))
	g.POST("/delete", taskDeleteHandler(manager))
	g.POST("/retry", taskRetryHandler(manager))
	g.POST("/cancel_some", taskCancelSomeHandler(manager))
	g.POST("/delete_some", taskDeleteSomeHandler(manager))
	g.POST("/retry_some", taskRetrySomeHandler(manager))
	g.POST("/clear_done", taskClearDoneHandler(manager))
	g.POST("/clear_succeeded", taskClearSucceededHandler(manager))
	g.POST("/retry_failed", taskRetryFailedHandler(manager))
}

// taskUndoneHandler lists the caller's not-yet-finished tasks (all tasks if admin).
// @Summary List undone tasks
// @Description Returns tasks in a not-yet-finished state (pending/running/canceling/errored/failing/waiting_retry/before_retry). Admins see every user's tasks; other users see only their own.
// @Tags task
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[[]TaskInfo]
// @Failure 401 {object} common.Resp[any]
// @Router /task/upload/undone [get]
// @Router /task/copy/undone [get]
// @Router /admin/task/upload/undone [get]
// @Router /admin/task/copy/undone [get]
// @Router /v4/task/upload/undone [get]
// @Router /v4/task/copy/undone [get]
// @Router /v4/admin/task/upload/undone [get]
// @Router /v4/admin/task/copy/undone [get]
func taskUndoneHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, uid, ok := getUserInfo(c)
		if !ok {
			// if there is no bug, here is unreachable
			common.ErrorI18n(c, "task.user_invalid", 401)
			return
		}
		common.SuccessResp(c, getTaskInfos(c, manager.GetByCondition(func(task T) bool {
			// avoid directly passing the user object into the function to reduce closure size
			return (isAdmin || uid == task.GetCreator().ID) &&
				argsContains(task.GetState(), tache.StatePending, tache.StateRunning, tache.StateCanceling,
					tache.StateErrored, tache.StateFailing, tache.StateWaitingRetry, tache.StateBeforeRetry)
		})))
	}
}

// taskDoneHandler lists the caller's finished tasks (all tasks if admin).
// @Summary List done tasks
// @Description Returns tasks in a finished state (canceled/failed/succeeded). Admins see every user's tasks; other users see only their own.
// @Tags task
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[[]TaskInfo]
// @Failure 401 {object} common.Resp[any]
// @Router /task/upload/done [get]
// @Router /task/copy/done [get]
// @Router /admin/task/upload/done [get]
// @Router /admin/task/copy/done [get]
// @Router /v4/task/upload/done [get]
// @Router /v4/task/copy/done [get]
// @Router /v4/admin/task/upload/done [get]
// @Router /v4/admin/task/copy/done [get]
func taskDoneHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, uid, ok := getUserInfo(c)
		if !ok {
			// if there is no bug, here is unreachable
			common.ErrorI18n(c, "task.user_invalid", 401)
			return
		}
		common.SuccessResp(c, getTaskInfos(c, manager.GetByCondition(func(task T) bool {
			return (isAdmin || uid == task.GetCreator().ID) &&
				argsContains(task.GetState(), tache.StateCanceled, tache.StateFailed, tache.StateSucceeded)
		})))
	}
}

// taskInfoHandler returns a single task's info. Non-admins may only look up their own tasks.
// @Summary Get task info
// @Tags task
// @Produce json
// @Security ApiKeyAuth
// @Param tid query string true "task id"
// @Success 200 {object} common.Resp[TaskInfo]
// @Failure 401 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any] "task not found (also returned instead of 403 to avoid leaking valid task ids)"
// @Router /task/upload/info [post]
// @Router /task/copy/info [post]
// @Router /admin/task/upload/info [post]
// @Router /admin/task/copy/info [post]
// @Router /v4/task/upload/info [post]
// @Router /v4/task/copy/info [post]
// @Router /v4/admin/task/upload/info [post]
// @Router /v4/admin/task/copy/info [post]
func taskInfoHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return getTargetedHandler(manager, func(c *gin.Context, task T) {
		common.SuccessResp(c, getTaskInfo(c, task))
	})
}

// taskCancelHandler cancels a single task. Non-admins may only cancel their own tasks.
// @Summary Cancel task
// @Tags task
// @Produce json
// @Security ApiKeyAuth
// @Param tid query string true "task id"
// @Success 200 {object} common.Resp[any]
// @Failure 401 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any] "task not found"
// @Router /task/upload/cancel [post]
// @Router /task/copy/cancel [post]
// @Router /admin/task/upload/cancel [post]
// @Router /admin/task/copy/cancel [post]
// @Router /v4/task/upload/cancel [post]
// @Router /v4/task/copy/cancel [post]
// @Router /v4/admin/task/upload/cancel [post]
// @Router /v4/admin/task/copy/cancel [post]
func taskCancelHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return getTargetedHandler(manager, func(c *gin.Context, task T) {
		manager.Cancel(task.GetID())
		common.SuccessResp(c)
	})
}

// taskDeleteHandler deletes a single task. Non-admins may only delete their own tasks.
// @Summary Delete task
// @Tags task
// @Produce json
// @Security ApiKeyAuth
// @Param tid query string true "task id"
// @Success 200 {object} common.Resp[any]
// @Failure 401 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any] "task not found"
// @Router /task/upload/delete [post]
// @Router /task/copy/delete [post]
// @Router /admin/task/upload/delete [post]
// @Router /admin/task/copy/delete [post]
// @Router /v4/task/upload/delete [post]
// @Router /v4/task/copy/delete [post]
// @Router /v4/admin/task/upload/delete [post]
// @Router /v4/admin/task/copy/delete [post]
func taskDeleteHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return getTargetedHandler(manager, func(c *gin.Context, task T) {
		manager.Remove(task.GetID())
		common.SuccessResp(c)
	})
}

// taskRetryHandler retries a single task. Non-admins may only retry their own tasks.
// @Summary Retry task
// @Tags task
// @Produce json
// @Security ApiKeyAuth
// @Param tid query string true "task id"
// @Success 200 {object} common.Resp[any]
// @Failure 401 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any] "task not found"
// @Router /task/upload/retry [post]
// @Router /task/copy/retry [post]
// @Router /admin/task/upload/retry [post]
// @Router /admin/task/copy/retry [post]
// @Router /v4/task/upload/retry [post]
// @Router /v4/task/copy/retry [post]
// @Router /v4/admin/task/upload/retry [post]
// @Router /v4/admin/task/copy/retry [post]
func taskRetryHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return getTargetedHandler(manager, func(c *gin.Context, task T) {
		manager.Retry(task.GetID())
		common.SuccessResp(c)
	})
}

// taskCancelSomeHandler cancels a batch of tasks by id.
// @Summary Cancel tasks (batch)
// @Description Cancels each task id in the body. Ids that don't exist, or aren't owned by a non-admin caller, are reported in the response map rather than failing the whole request.
// @Tags task
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body []string true "task ids"
// @Success 200 {object} common.Resp[map[string]string] "data: map of task id -> error message, only for ids that failed"
// @Failure 401 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid request format"
// @Router /task/upload/cancel_some [post]
// @Router /task/copy/cancel_some [post]
// @Router /admin/task/upload/cancel_some [post]
// @Router /admin/task/copy/cancel_some [post]
// @Router /v4/task/upload/cancel_some [post]
// @Router /v4/task/copy/cancel_some [post]
// @Router /v4/admin/task/upload/cancel_some [post]
// @Router /v4/admin/task/copy/cancel_some [post]
func taskCancelSomeHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return getBatchHandler(manager, func(task T) {
		manager.Cancel(task.GetID())
	})
}

// taskDeleteSomeHandler deletes a batch of tasks by id.
// @Summary Delete tasks (batch)
// @Description Deletes each task id in the body. Ids that don't exist, or aren't owned by a non-admin caller, are reported in the response map rather than failing the whole request.
// @Tags task
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body []string true "task ids"
// @Success 200 {object} common.Resp[map[string]string] "data: map of task id -> error message, only for ids that failed"
// @Failure 401 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid request format"
// @Router /task/upload/delete_some [post]
// @Router /task/copy/delete_some [post]
// @Router /admin/task/upload/delete_some [post]
// @Router /admin/task/copy/delete_some [post]
// @Router /v4/task/upload/delete_some [post]
// @Router /v4/task/copy/delete_some [post]
// @Router /v4/admin/task/upload/delete_some [post]
// @Router /v4/admin/task/copy/delete_some [post]
func taskDeleteSomeHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return getBatchHandler(manager, func(task T) {
		manager.Remove(task.GetID())
	})
}

// taskRetrySomeHandler retries a batch of tasks by id.
// @Summary Retry tasks (batch)
// @Description Retries each task id in the body. Ids that don't exist, or aren't owned by a non-admin caller, are reported in the response map rather than failing the whole request.
// @Tags task
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body []string true "task ids"
// @Success 200 {object} common.Resp[map[string]string] "data: map of task id -> error message, only for ids that failed"
// @Failure 401 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid request format"
// @Router /task/upload/retry_some [post]
// @Router /task/copy/retry_some [post]
// @Router /admin/task/upload/retry_some [post]
// @Router /admin/task/copy/retry_some [post]
// @Router /v4/task/upload/retry_some [post]
// @Router /v4/task/copy/retry_some [post]
// @Router /v4/admin/task/upload/retry_some [post]
// @Router /v4/admin/task/copy/retry_some [post]
func taskRetrySomeHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return getBatchHandler(manager, func(task T) {
		manager.Retry(task.GetID())
	})
}

// taskClearDoneHandler removes all of the caller's finished tasks (all users' if admin).
// @Summary Clear done tasks
// @Description Removes all finished tasks (canceled/failed/succeeded) belonging to the caller (or every user's, if admin).
// @Tags task
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[any]
// @Failure 401 {object} common.Resp[any]
// @Router /task/upload/clear_done [post]
// @Router /task/copy/clear_done [post]
// @Router /admin/task/upload/clear_done [post]
// @Router /admin/task/copy/clear_done [post]
// @Router /v4/task/upload/clear_done [post]
// @Router /v4/task/copy/clear_done [post]
// @Router /v4/admin/task/upload/clear_done [post]
// @Router /v4/admin/task/copy/clear_done [post]
func taskClearDoneHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, uid, ok := getUserInfo(c)
		if !ok {
			// if there is no bug, here is unreachable
			common.ErrorI18n(c, "task.user_invalid", 401)
			return
		}
		manager.RemoveByCondition(func(task T) bool {
			return (isAdmin || uid == task.GetCreator().ID) &&
				argsContains(task.GetState(), tache.StateCanceled, tache.StateFailed, tache.StateSucceeded)
		})
		common.SuccessResp(c)
	}
}

// taskClearSucceededHandler removes all of the caller's succeeded tasks (all users' if admin).
// @Summary Clear succeeded tasks
// @Description Removes all succeeded tasks belonging to the caller (or every user's, if admin).
// @Tags task
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[any]
// @Failure 401 {object} common.Resp[any]
// @Router /task/upload/clear_succeeded [post]
// @Router /task/copy/clear_succeeded [post]
// @Router /admin/task/upload/clear_succeeded [post]
// @Router /admin/task/copy/clear_succeeded [post]
// @Router /v4/task/upload/clear_succeeded [post]
// @Router /v4/task/copy/clear_succeeded [post]
// @Router /v4/admin/task/upload/clear_succeeded [post]
// @Router /v4/admin/task/copy/clear_succeeded [post]
func taskClearSucceededHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, uid, ok := getUserInfo(c)
		if !ok {
			// if there is no bug, here is unreachable
			common.ErrorI18n(c, "task.user_invalid", 401)
			return
		}
		manager.RemoveByCondition(func(task T) bool {
			return (isAdmin || uid == task.GetCreator().ID) && task.GetState() == tache.StateSucceeded
		})
		common.SuccessResp(c)
	}
}

// taskRetryFailedHandler retries all of the caller's failed tasks (all users' if admin).
// @Summary Retry failed tasks
// @Description Retries every failed task belonging to the caller (or every user's, if admin).
// @Tags task
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} common.Resp[any]
// @Failure 401 {object} common.Resp[any]
// @Router /task/upload/retry_failed [post]
// @Router /task/copy/retry_failed [post]
// @Router /admin/task/upload/retry_failed [post]
// @Router /admin/task/copy/retry_failed [post]
// @Router /v4/task/upload/retry_failed [post]
// @Router /v4/task/copy/retry_failed [post]
// @Router /v4/admin/task/upload/retry_failed [post]
// @Router /v4/admin/task/copy/retry_failed [post]
func taskRetryFailedHandler[T task.TaskExtensionInfo](manager task.Manager[T]) gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, uid, ok := getUserInfo(c)
		if !ok {
			// if there is no bug, here is unreachable
			common.ErrorI18n(c, "task.user_invalid", 401)
			return
		}
		tasks := manager.GetByCondition(func(task T) bool {
			return (isAdmin || uid == task.GetCreator().ID) && task.GetState() == tache.StateFailed
		})
		for _, t := range tasks {
			manager.Retry(t.GetID())
		}
		common.SuccessResp(c)
	}
}

func SetupTaskRoute(g *gin.RouterGroup) {
	taskRoute(g.Group("/upload"), fs.UploadTaskManager)
	taskRoute(g.Group("/copy"), fs.CopyTaskManager)
	taskRoute(g.Group("/move"), fs.MoveTaskManager)
	taskRoute(g.Group("/offline_download"), offline_download.OfflineDownloadTaskManager)
}
