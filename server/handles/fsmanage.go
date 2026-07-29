package handles

import (
	"fmt"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"io"
	stdpath "path"

	"github.com/alist-org/alist/v3/internal/task"

	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/generic"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type MkdirOrLinkReq struct {
	Path string `json:"path" form:"path"`
}

// FsMkdir creates a directory (and any missing parents) at path.
// @Summary Create directory
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body MkdirOrLinkReq true "path"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "permission denied"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/mkdir [post]
// @Router /v4/fs/mkdir [post]
func FsMkdir(c *gin.Context) {
	var req MkdirOrLinkReq
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
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}
	if err := fs.MakeDir(c, reqPath); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

type MoveCopyReq struct {
	SrcDir    string   `json:"src_dir"`
	DstDir    string   `json:"dst_dir"`
	Names     []string `json:"names"`
	Overwrite bool     `json:"overwrite"`
	// PermPolicy applies to move only (spec 0-C): "" / "follow" (default) keeps
	// manual permissions following the object; "detach" drops them from the
	// moved subtree.
	PermPolicy string `json:"perm_policy"`
	// CopyPerms applies to copy only: clone the source subtree's manual
	// permissions onto the destination. Default false.
	CopyPerms bool `json:"copy_perms"`
}

// authorizePermissionManage gates the manual-permission side effects of the
// move/copy perm options behind the system:permission manage action. It is an
// extra check ON TOP of the fs action authorization, run only when a non-default
// option is requested, so the request is rejected (not silently downgraded) when
// the caller cannot manage permissions. Admins short-circuit exactly like
// middlewares.RequireSystem: op.Authorize has no admin bypass and the admin role
// carries only fs scope, so without this an admin would be denied the option.
func authorizePermissionManage(c *gin.Context, user *model.User) error {
	if user.IsAdmin() {
		return nil
	}
	return op.Authorize(c, user, model.SystemAction("permission", "manage"),
		op.AuthzRequest{ResourceType: model.ResourceSystem, Path: "permission"})
}

// parsePermPolicy validates the move perm_policy option and reports whether it
// selects detach. "" and "follow" are the default (follow); "detach" opts into
// dropping the moved subtree's manual permissions; anything else is a 400.
func parsePermPolicy(policy string) (detach bool, err error) {
	switch policy {
	case "", model.PermPolicyFollow:
		return false, nil
	case model.PermPolicyDetach:
		return true, nil
	default:
		return false, errors.Errorf("invalid perm_policy %q (want %q or %q)",
			policy, model.PermPolicyFollow, model.PermPolicyDetach)
	}
}

// isCrossStorageMove reports whether a move from srcDir into dstDir crosses a
// storage boundary, using the same storage-identity comparison moveAsTask makes
// per object. Both paths are already base-path joined.
func isCrossStorageMove(srcDir, dstDir string) (bool, error) {
	srcStorage, _, err := op.GetStorageAndActualPath(srcDir)
	if err != nil {
		return false, err
	}
	dstStorage, _, err := op.GetStorageAndActualPath(dstDir)
	if err != nil {
		return false, err
	}
	return srcStorage.GetStorage() != dstStorage.GetStorage(), nil
}

// FsMove moves one or more named entries from src_dir into dst_dir as background tasks.
// @Summary Move files/directories
// @Description Requires ActMove on src_dir and on each source entry, plus ActWrite on dst_dir. Response data.tasks lists the created background move tasks.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body MoveCopyReq true "src_dir/dst_dir/names/overwrite"
// @Success 200 {object} common.Resp[gin.H] "data: {tasks: TaskInfo[]}"
// @Failure 400 {object} common.Resp[any] "bad request or empty names"
// @Failure 403 {object} common.Resp[any] "permission denied or destination file exists"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/move [post]
// @Router /v4/fs/move [post]
func FsMove(c *gin.Context) {
	var req MoveCopyReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if len(req.Names) == 0 {
		common.ErrorI18n(c, "fsmanage.empty_file_names", 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	srcDir, err := user.JoinPath(req.SrcDir)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	dstDir, err := user.JoinPath(req.DstDir)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActMove, op.AuthzRequest{Path: srcDir}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}
	// srcDir only proves the caller may take content out of srcDir; a move
	// also writes into dstDir, which needs its own authorization or a user
	// with no rights there could have content moved into it regardless.
	if err := op.Authorize(c, user, model.ActWrite, op.AuthzRequest{Path: dstDir}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}
	detach, err := parsePermPolicy(req.PermPolicy)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if detach {
		// A non-default perm policy edits manual permission rows, so it needs
		// system:permission manage on top of the fs move authorization. Reject
		// (403) rather than silently ignore, so the caller never believes the
		// option took effect when it did not.
		if err := authorizePermissionManage(c, user); err != nil {
			common.AuthzErrorResp(c, err, "permission denied")
			return
		}
		// Cross-storage move is copy+delete: the relocate hook never fires, so
		// there is nothing for detach to act on. Fail explicitly (spec 0-C
		// leaves cross-storage follow to a later stage) rather than accept an
		// option that would be a no-op.
		if crossStorage, err := isCrossStorageMove(srcDir, dstDir); err != nil {
			common.ErrorResp(c, err, 400)
			return
		} else if crossStorage {
			common.ErrorResp(c, errors.New("perm_policy=detach is not supported for a cross-storage move"), 400)
			return
		}
	}
	if !req.Overwrite {
		for _, name := range req.Names {
			dstPath, err := utils.JoinUnderBase(dstDir, name)
			if err != nil {
				common.ErrorResp(c, err, 400)
				return
			}
			if res, _ := fs.Get(c, dstPath, &fs.GetArgs{NoLog: true}); res != nil {
				common.ErrorI18n(c, "fsmanage.file_exists", 403, "name", name)
				return
			}
		}
	}
	var addedTasks []task.TaskExtensionInfo
	for _, name := range req.Names {
		srcPath, err := utils.JoinUnderBase(srcDir, name)
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		_, err = utils.JoinUnderBase(dstDir, name)
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		// The dir-level check above only proves ActMove on srcDir; a manual
		// recursive deny nested under srcDir (e.g. srcDir/name) is never
		// consulted unless we also authorize the per-target path.
		if err := op.Authorize(c, user, model.ActMove, op.AuthzRequest{Path: srcPath}); err != nil {
			common.AuthzErrorResp(c, err, "permission denied")
			return
		}
		t, err := fs.MoveAsTask(c, srcPath, dstDir, detach)
		if t != nil {
			addedTasks = append(addedTasks, t)
		}
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
	}
	common.SuccessResp(c, gin.H{
		"tasks": getTaskInfos(c, addedTasks),
	})
}

// FsCopy copies one or more named entries from src_dir into dst_dir, possibly as background tasks.
// @Summary Copy files/directories
// @Description Requires ActCopy on src_dir and on each source entry, plus ActWrite on dst_dir. Response data.tasks lists any created background copy tasks.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body MoveCopyReq true "src_dir/dst_dir/names/overwrite"
// @Success 200 {object} common.Resp[gin.H] "data: {tasks: TaskInfo[]}"
// @Failure 400 {object} common.Resp[any] "bad request or empty names"
// @Failure 403 {object} common.Resp[any] "permission denied or destination file exists"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/copy [post]
// @Router /v4/fs/copy [post]
func FsCopy(c *gin.Context) {
	var req MoveCopyReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if len(req.Names) == 0 {
		common.ErrorI18n(c, "fsmanage.empty_file_names", 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	srcDir, err := user.JoinPath(req.SrcDir)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	dstDir, err := user.JoinPath(req.DstDir)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActCopy, op.AuthzRequest{Path: srcDir}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}
	// srcDir only proves the caller may read content out of srcDir; a copy
	// also writes into dstDir, which needs its own authorization or a user
	// with no rights there could have content copied into it regardless.
	if err := op.Authorize(c, user, model.ActWrite, op.AuthzRequest{Path: dstDir}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}
	if req.CopyPerms {
		// Cloning manual permissions needs system:permission manage on top of
		// the fs copy authorization; reject rather than silently ignore. Copy
		// is not restricted across storages — the clone re-anchors by path.
		if err := authorizePermissionManage(c, user); err != nil {
			common.AuthzErrorResp(c, err, "permission denied")
			return
		}
	}
	if !req.Overwrite {
		for _, name := range req.Names {
			dstPath, err := utils.JoinUnderBase(dstDir, name)
			if err != nil {
				common.ErrorResp(c, err, 400)
				return
			}
			if res, _ := fs.Get(c, dstPath, &fs.GetArgs{NoLog: true}); res != nil {
				common.ErrorI18n(c, "fsmanage.file_exists", 403, "name", name)
				return
			}
		}
	}
	var addedTasks []task.TaskExtensionInfo
	for i, name := range req.Names {
		srcPath, err := utils.JoinUnderBase(srcDir, name)
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		_, err = utils.JoinUnderBase(dstDir, name)
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		// The dir-level check above only proves ActCopy on srcDir; a manual
		// recursive deny nested under srcDir (e.g. srcDir/name) is never
		// consulted unless we also authorize the per-target path.
		if err := op.Authorize(c, user, model.ActCopy, op.AuthzRequest{Path: srcPath}); err != nil {
			common.AuthzErrorResp(c, err, "permission denied")
			return
		}
		t, err := fs.Copy(c, srcPath, dstDir, req.CopyPerms, len(req.Names) > i+1)
		if t != nil {
			addedTasks = append(addedTasks, t)
		}
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
	}
	common.SuccessResp(c, gin.H{
		"tasks": getTaskInfos(c, addedTasks),
	})
}

type RenameReq struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Overwrite bool   `json:"overwrite"`
	// PermPolicy: "" / "follow" (default) keeps manual permissions following the
	// renamed object; "detach" drops them from the renamed subtree (spec 0-C).
	PermPolicy string `json:"perm_policy"`
}

func canRenamePath(c *gin.Context, reqPath string) bool {
	em, err := op.ResolveEffectiveMeta(reqPath)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return false
	}
	if em.PasswordApplies {
		common.ErrorI18n(c, "fsmanage.path_password_protected_rename", 403)
		return false
	}
	return true
}

// FsRename renames a single file/directory in place.
// @Summary Rename file/directory
// @Description Fails if path is password-protected (its meta has PasswordApplies). Requires ActRename on path.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body RenameReq true "path/name/overwrite"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "bad request or invalid name"
// @Failure 403 {object} common.Resp[any] "permission denied, password-protected, or destination exists"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/rename [post]
// @Router /v4/fs/rename [post]
func FsRename(c *gin.Context) {
	var req RenameReq
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
	if !canRenamePath(c, reqPath) {
		return
	}
	if err := op.Authorize(c, user, model.ActRename, op.AuthzRequest{Path: reqPath}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}
	detach, err := parsePermPolicy(req.PermPolicy)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if detach {
		// Editing manual permission rows needs system:permission manage on top
		// of the fs rename authorization; reject rather than silently ignore.
		if err := authorizePermissionManage(c, user); err != nil {
			common.AuthzErrorResp(c, err, "permission denied")
			return
		}
	}
	if err := utils.ValidateNameComponent(req.Name); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if !req.Overwrite {
		dstPath, err := utils.JoinUnderBase(stdpath.Dir(reqPath), req.Name)
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		if dstPath != reqPath {
			if res, _ := fs.Get(c, dstPath, &fs.GetArgs{NoLog: true}); res != nil {
				common.ErrorI18n(c, "fsmanage.file_exists", 403, "name", req.Name)
				return
			}
		}
	}
	// Rename is in-place (always same storage) and synchronous: op.Rename fires
	// the follow hook inline, so capture the manual rows before the rename and
	// delete them after it succeeds — a failed rename deletes nothing.
	var detachIDs []uint
	if detach {
		storage, actualPath, err := op.GetStorageAndActualPath(reqPath)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		detachIDs, err = op.CaptureManualPermissionIDs(c, storage, actualPath)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
	}
	if err := fs.Rename(c, reqPath, req.Name); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if detach {
		if err := op.DeleteDetachedPermissions(detachIDs); err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
	}
	common.SuccessResp(c)
}

type RemoveReq struct {
	Dir   string   `json:"dir"`
	Names []string `json:"names"`
}

// FsRemove deletes one or more named entries from dir.
// @Summary Remove files/directories
// @Description Requires ActRemove on dir and on each target entry.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body RemoveReq true "dir/names"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "bad request or empty names"
// @Failure 403 {object} common.Resp[any] "permission denied"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/remove [post]
// @Router /v4/fs/remove [post]
func FsRemove(c *gin.Context) {
	var req RemoveReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if len(req.Names) == 0 {
		common.ErrorI18n(c, "fsmanage.empty_file_names", 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqDir, err := user.JoinPath(req.Dir)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActRemove, op.AuthzRequest{Path: reqDir}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}
	for _, name := range req.Names {
		removePath, err := utils.JoinUnderBase(reqDir, name)
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		// The dir-level check above only proves ActRemove on reqDir; a
		// manual recursive deny nested under reqDir (e.g. reqDir/name) is
		// never consulted unless we also authorize the per-target path.
		if err := op.Authorize(c, user, model.ActRemove, op.AuthzRequest{Path: removePath}); err != nil {
			common.AuthzErrorResp(c, err, "permission denied")
			return
		}
		err = fs.Remove(c, removePath)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
	}
	//fs.ClearCache(req.Dir)
	common.SuccessResp(c)
}

type RemoveEmptyDirectoryReq struct {
	SrcDir string `json:"src_dir"`
}

// FsRemoveEmptyDirectory recursively removes empty subdirectories under src_dir.
// @Summary Remove empty subdirectories
// @Description Requires ActRemove on src_dir. Emptiness is checked against the unfiltered listing (not permission/hide-filtered), so a directory whose children are merely hidden from the caller is not treated as empty.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body RemoveEmptyDirectoryReq true "src_dir"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "permission denied"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/remove_empty_directory [post]
// @Router /v4/fs/remove_empty_directory [post]
func FsRemoveEmptyDirectory(c *gin.Context) {
	var req RemoveEmptyDirectoryReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	user := c.MustGet(ctxkey.User).(*model.User)
	srcDir, err := user.JoinPath(req.SrcDir)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActRemove, op.AuthzRequest{Path: srcDir}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}

	rootFiles, err := fs.List(c, srcDir, &fs.ListArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	// record the file path
	filePathMap := make(map[model.Obj]string)
	// record the parent file
	fileParentMap := make(map[model.Obj]model.Obj)
	// removing files
	removingFiles := generic.NewQueue[model.Obj]()
	// removed files
	removedFiles := make(map[string]bool)
	for _, file := range rootFiles {
		if !file.IsDir() {
			continue
		}
		removingFiles.Push(file)
		filePathMap[file] = srcDir
	}

	for !removingFiles.IsEmpty() {

		removingFile := removingFiles.Pop()
		removingFilePath := fmt.Sprintf("%s/%s", filePathMap[removingFile], removingFile.GetName())

		if removedFiles[removingFilePath] {
			continue
		}

		subFiles, err := fs.List(c, removingFilePath, &fs.ListArgs{Refresh: true})
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}

		// Emptiness is a factual predicate: this listing must NOT be permission-
		// or hide-filtered, or a directory whose children are merely invisible to
		// the caller would be "empty" and physically deleted (data loss).
		if len(subFiles) == 0 {
			// remove empty directory
			err = fs.Remove(c, removingFilePath)
			removedFiles[removingFilePath] = true
			if err != nil {
				common.ErrorResp(c, err, 500)
				return
			}
			// recheck parent folder
			parentFile, exist := fileParentMap[removingFile]
			if exist {
				removingFiles.Push(parentFile)
			}

		} else {
			// recursive remove
			for _, subFile := range subFiles {
				if !subFile.IsDir() {
					continue
				}
				removingFiles.Push(subFile)
				filePathMap[subFile] = removingFilePath
				fileParentMap[subFile] = removingFile
			}
		}

	}

	common.SuccessResp(c)
}

// Link returns the real (storage-native) link for a path, which may include cookies/headers, for use by proxy programs.
// @Summary Get raw storage link (admin only)
// @Description Admin-only (mounted behind middlewares.AuthAdmin in the router). rawPath is used as-is, not joined with the caller's base path.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body MkdirOrLinkReq true "path"
// @Success 200 {object} common.Resp[model.Link]
// @Failure 400 {object} common.Resp[any]
// @Failure 500 {object} common.Resp[any]
// @Router /fs/link [post]
// @Router /v4/admin/fs/link [post]
func Link(c *gin.Context) {
	var req MkdirOrLinkReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	rawPath := req.Path
	storage, err := fs.GetStorage(rawPath, &fs.GetStoragesArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if storage.Config().OnlyLocal {
		common.SuccessResp(c, model.Link{
			URL: fmt.Sprintf("%s/p%s?d&sign=%s",
				common.GetApiUrl(c.Request),
				utils.EncodePath(rawPath, true),
				common.SignPath(rawPath)),
		})
		return
	}
	link, _, err := fs.Link(c, rawPath, model.LinkArgs{IP: c.ClientIP(), Header: c.Request.Header, HttpReq: c.Request})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.CompleteLink(c.Request, link)
	if link.MFile != nil {
		defer func(ReadSeekCloser io.ReadCloser) {
			err := ReadSeekCloser.Close()
			if err != nil {
				log.Errorf("close link data error: %v", err)
			}
		}(link.MFile)
	}
	common.SuccessResp(c, link)
	return
}
