package handles

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/offline_download"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// AddOfflineDownloadReq is the add-offline-download body. Tool selects the
// download backend: empty falls back to the offline_download_tool setting
// (default "SimpleHttp"); "SimpleHttp" forces the built-in direct path; any
// other value routes the batch through the matching daemon tool extension.
type AddOfflineDownloadReq struct {
	Path string   `json:"path"`
	URLs []string `json:"urls"`
	Tool string   `json:"tool"`
}

// defaultToolName reports the tool a fresh offline-download dialog should
// preselect: the offline_download_tool setting (default "SimpleHttp"), but
// normalized back to "SimpleHttp" when that setting names a tool that is not
// currently available (unregistered / disabled owning extension), so the client
// never preselects an option the picker won't list. Membership is checked
// against AvailableToolNames — the exact set the endpoint returns — which always
// contains "SimpleHttp".
func defaultToolName() string {
	name := setting.GetStr(conf.OfflineDownloadTool, "SimpleHttp")
	for _, t := range offline_download.AvailableToolNames() {
		if t == name {
			return name
		}
	}
	return "SimpleHttp"
}

// ListOfflineDownloadTools returns the offline-download tools the caller may
// pick for an add — AvailableToolNames (SimpleHttp first, then every enabled
// registered tool) plus the default the dialog should preselect. It is an authed
// read gated by the same builtin.offline_download feature as add_offline_download,
// so any logged-in user with the offline-download feature can populate the picker.
// @Summary List offline-download tools
// @Description Returns the offline-download tools this caller may select (SimpleHttp plus every enabled daemon tool) and the tool the picker should preselect (the offline_download_tool setting, normalized back to SimpleHttp if that setting names an unavailable tool).
// @Tags offline-download
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {object} common.Resp[gin.H] "data: {tools: []string, default: string}"
// @Failure 401 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "builtin.offline_download disabled"
// @Router /v4/fs/offline_download/tools [get]
func ListOfflineDownloadTools(c *gin.Context) {
	common.SuccessResp(c, gin.H{
		"tools":   offline_download.AvailableToolNames(),
		"default": defaultToolName(),
	})
}

// AddOfflineDownload queues one background DownloadTask per URL, targeting the
// directory at req.Path in the caller's virtual filesystem.
//
// Authorization is a DOUBLE gate on the target path, and the order is
// deliberate: ActWrite first (creating a file there is the root security
// boundary — no write, no business asking about offline downloads), then
// ActOfflineDownload (the capability bit that specifically permits pulling a
// remote URL server-side). Missing either is a 403. Both run before any task is
// created, so a denied caller never queues work.
//
// URL validation is all-or-nothing: the whole batch is rejected (400) the
// moment any entry is not a syntactically valid http/https URL, so a caller
// never gets a half-applied submission where some URLs queued and one silently
// vanished. Blank entries (after trimming) are skipped, not rejected. The
// SSRF-safe fetch in the task layer re-checks the resolved IP at dial time; the
// scheme check here is only the cheap up-front guard.
//
// Tool selection: req.Tool wins; empty falls back to the offline_download_tool
// setting (default "SimpleHttp"). "SimpleHttp"/"none"/empty resolution builds the
// built-in direct-download task (unchanged); any other name builds daemon tasks
// (Toolname set, TempDir under conf.TempDir/<tool>). Availability of a daemon
// tool depends on WHO named it: a tool the CALLER explicitly named but that is
// unavailable/disabled is a 400 (they asked for something we can't do), whereas a
// stale/unavailable SETTING default silently falls back to SimpleHttp so a plain
// add never 400s just because an admin later disabled the default daemon.
// @Summary Queue an offline download
// @Description Queues one background download task per URL into the target directory (path is joined with the caller's base path). Requires ActWrite and ActOfflineDownload on path. tool selects the download backend; empty falls back to the offline_download_tool setting (default SimpleHttp).
// @Tags offline-download
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body AddOfflineDownloadReq true "path/urls/tool"
// @Success 200 {object} common.Resp[[]TaskInfo]
// @Failure 400 {object} common.Resp[any] "bad request, no urls, invalid url, or explicitly named tool unavailable"
// @Failure 401 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "permission denied or builtin.offline_download disabled"
// @Failure 500 {object} common.Resp[any]
// @Router /v4/fs/add_offline_download [post]
func AddOfflineDownload(c *gin.Context) {
	var req AddOfflineDownloadReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	// Double Authorize: write permission (the root boundary for creating a file)
	// then the offline-download capability bit. Either missing -> 403.
	if err := op.Authorize(c, user, model.ActWrite, op.AuthzRequest{Path: reqPath}); err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActOfflineDownload, op.AuthzRequest{Path: reqPath}); err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if len(req.URLs) == 0 {
		common.ErrorResp(c, errors.New("no urls"), 400)
		return
	}
	storage, actual, err := op.GetStorageAndActualPath(reqPath)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	// Validate the ENTIRE batch before queuing anything: all-or-nothing means a
	// single bad URL rejects the whole submission, so a valid URL earlier in the
	// list must not have already been queued when a later one fails. Two passes:
	// collect+validate first, then Add only once every URL has passed.
	var clean []string
	for _, raw := range req.URLs {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue // blank entries are skipped, not an error
		}
		pu, e := url.Parse(u)
		if e != nil || (pu.Scheme != "http" && pu.Scheme != "https") {
			common.ErrorResp(c, fmt.Errorf("invalid url %q (only http/https)", raw), 400)
			return
		}
		clean = append(clean, u)
	}
	if len(clean) == 0 {
		common.ErrorResp(c, errors.New("no urls"), 400)
		return
	}

	// Resolve the effective tool. explicitTool records whether the caller NAMED
	// it: that distinguishes an explicit-unavailable (400) from a stale-default
	// (silent SimpleHttp fallback) below.
	explicitTool := strings.TrimSpace(req.Tool) != ""
	toolName := strings.TrimSpace(req.Tool)
	if toolName == "" {
		toolName = setting.GetStr(conf.OfflineDownloadTool, "SimpleHttp")
	}
	daemon := false
	if toolName != "" && toolName != "SimpleHttp" && toolName != "none" {
		if offline_download.ToolByName(toolName) == nil {
			// The named daemon tool is unregistered/disabled.
			if explicitTool {
				// The caller asked for this specific tool: refuse rather than
				// silently downloading via a different mechanism.
				common.ErrorResp(c, fmt.Errorf("offline download tool %q is not available or not enabled", toolName), 400)
				return
			}
			// Stale/unavailable SETTING default: fall back to SimpleHttp so a plain
			// add still succeeds.
			toolName = "SimpleHttp"
		} else {
			daemon = true
		}
	}

	mountPath := storage.GetStorage().MountPath
	infos := make([]TaskInfo, 0, len(clean))
	for _, u := range clean {
		var t *offline_download.DownloadTask
		if daemon {
			// Daemon path: Toolname set + a per-download TempDir
			// (conf.TempDir/<tool>/<uuid>) the tool downloads into and the transfer
			// engine later drains into the destination storage.
			t = offline_download.NewDaemonDownloadTask(toolName, u, mountPath, actual)
		} else {
			// SimpleHttp direct path: Toolname empty, unchanged behavior.
			t = &offline_download.DownloadTask{
				URL:              u,
				DstStorageMp:     mountPath,
				DstDirActualPath: actual,
			}
		}
		t.SetCreator(user)
		offline_download.OfflineDownloadTaskManager.Add(t)
		infos = append(infos, getTaskInfo(c, t))
	}
	common.SuccessResp(c, infos)
}
