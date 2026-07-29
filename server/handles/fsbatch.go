package handles

import (
	"fmt"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"regexp"
	"slices"

	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/generic"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

type RecursiveMoveReq struct {
	SrcDir         string `json:"src_dir"`
	DstDir         string `json:"dst_dir"`
	ConflictPolicy string `json:"conflict_policy"`
}

// FsRecursiveMove flattens all files found recursively under src_dir into dst_dir, per conflict_policy.
// @Summary Recursively move files into a single destination
// @Description Requires ActMove on src_dir and on each source file, plus ActWrite on dst_dir. conflict_policy controls name clashes in dst_dir (e.g. "overwrite", "skip", "cancel").
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body RecursiveMoveReq true "src_dir/dst_dir/conflict_policy"
// @Success 200 {object} common.Resp[any] "message: Successfully moved N file(s)"
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "permission denied or file exists (cancel policy)"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/recursive_move [post]
// @Router /v4/fs/recursive_move [post]
func FsRecursiveMove(c *gin.Context) {
	var req RecursiveMoveReq
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
	dstDir, err := user.JoinPath(req.DstDir)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActMove, op.AuthzRequest{Path: srcDir}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}
	// srcDir only proves the caller may take content out of srcDir; a
	// recursive move also writes into dstDir, which needs its own
	// authorization or a user with no rights there could have content
	// moved into it regardless.
	if err := op.Authorize(c, user, model.ActWrite, op.AuthzRequest{Path: dstDir}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}

	rootFiles, err := fs.List(c, srcDir, &fs.ListArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	rootFiles = op.FilterListing(c, user, srcDir, rootFiles)

	var existingFileNames []string
	if req.ConflictPolicy != OVERWRITE {
		dstFiles, err := fs.List(c, dstDir, &fs.ListArgs{})
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		existingFileNames = make([]string, 0, len(dstFiles))
		for _, dstFile := range dstFiles {
			existingFileNames = append(existingFileNames, dstFile.GetName())
		}
	}

	// record the file path
	filePathMap := make(map[model.Obj]string)
	movingFiles := generic.NewQueue[model.Obj]()
	movingFileNames := make([]string, 0, len(rootFiles))
	for _, file := range rootFiles {
		movingFiles.Push(file)
		filePathMap[file] = srcDir
	}

	for !movingFiles.IsEmpty() {

		movingFile := movingFiles.Pop()
		movingFilePath := filePathMap[movingFile]
		movingFileName := fmt.Sprintf("%s/%s", movingFilePath, movingFile.GetName())
		if movingFile.IsDir() {
			// directory, recursive move
			subFilePath := movingFileName
			subFiles, err := fs.List(c, movingFileName, &fs.ListArgs{Refresh: true})
			if err != nil {
				common.ErrorResp(c, err, 500)
				return
			}
			subFiles = op.FilterListing(c, user, subFilePath, subFiles)
			for _, subFile := range subFiles {
				movingFiles.Push(subFile)
				filePathMap[subFile] = subFilePath
			}
		} else {
			if movingFilePath == dstDir {
				// same directory, don't move
				continue
			}

			if slices.Contains(existingFileNames, movingFile.GetName()) {
				if req.ConflictPolicy == CANCEL {
					common.ErrorStrResp(c, fmt.Sprintf("file [%s] exists", movingFile.GetName()), 403)
					return
				} else if req.ConflictPolicy == SKIP {
					continue
				}
			} else if req.ConflictPolicy != OVERWRITE {
				existingFileNames = append(existingFileNames, movingFile.GetName())
			}
			movingFileNames = append(movingFileNames, movingFileName)

		}

	}

	var count = 0
	for i, fileName := range movingFileNames {
		// FilterListing (used to build movingFileNames above) only excludes
		// on ActRead; a deny scoped to ActMove alone (read still allowed)
		// would stay visible in the listing and reach here undetected
		// without this per-file check.
		if err := op.Authorize(c, user, model.ActMove, op.AuthzRequest{Path: fileName}); err != nil {
			common.AuthzErrorResp(c, err, "permission denied")
			return
		}
		// move
		err := fs.Move(c, fileName, dstDir, len(movingFileNames) > i+1)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		count++
	}

	common.SuccessWithMsgResp(c, fmt.Sprintf("Successfully moved %d %s", count, common.Pluralize(count, "file", "files")))
}

type BatchRenameReq struct {
	SrcDir        string `json:"src_dir"`
	RenameObjects []struct {
		SrcName string `json:"src_name"`
		NewName string `json:"new_name"`
	} `json:"rename_objects"`
}

// FsBatchRename renames a batch of explicitly named entries under src_dir.
// @Summary Batch rename files/directories
// @Description Requires ActRename on src_dir and on each target entry. Skips entries with empty src_name or new_name. Fails if a target path is password-protected.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body BatchRenameReq true "src_dir/rename_objects[]"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "bad request or invalid name"
// @Failure 403 {object} common.Resp[any] "permission denied or password-protected"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/batch_rename [post]
// @Router /v4/fs/batch_rename [post]
func FsBatchRename(c *gin.Context) {
	var req BatchRenameReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(req.SrcDir)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActRename, op.AuthzRequest{Path: reqPath}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}

	for _, renameObject := range req.RenameObjects {
		if renameObject.SrcName == "" || renameObject.NewName == "" {
			continue
		}
		if err := utils.ValidateNameComponent(renameObject.NewName); err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		filePath, err := utils.JoinUnderBase(reqPath, renameObject.SrcName)
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		if !canRenamePath(c, filePath) {
			return
		}
		// The dir-level check above only proves ActRename on reqPath; a
		// manual recursive deny nested under reqPath (e.g. reqPath/name) is
		// never consulted unless we also authorize the per-target path.
		if err := op.Authorize(c, user, model.ActRename, op.AuthzRequest{Path: filePath}); err != nil {
			common.AuthzErrorResp(c, err, "permission denied")
			return
		}
		if err := fs.Rename(c, filePath, renameObject.NewName); err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
	}
	common.SuccessResp(c)
}

type RegexRenameReq struct {
	SrcDir       string `json:"src_dir"`
	SrcNameRegex string `json:"src_name_regex"`
	NewNameRegex string `json:"new_name_regex"`
}

// FsRegexRename renames every entry directly under src_dir whose name matches src_name_regex, substituting new_name_regex.
// @Summary Regex-based batch rename
// @Description Requires ActRename on src_dir and on each matched entry. Fails if a target path is password-protected.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body RegexRenameReq true "src_dir/src_name_regex/new_name_regex"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any] "invalid resulting name"
// @Failure 403 {object} common.Resp[any] "permission denied or password-protected"
// @Failure 500 {object} common.Resp[any] "invalid regex or list/rename failure"
// @Router /fs/regex_rename [post]
// @Router /v4/fs/regex_rename [post]
func FsRegexRename(c *gin.Context) {
	var req RegexRenameReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(req.SrcDir)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActRename, op.AuthzRequest{Path: reqPath}); err != nil {
		common.AuthzErrorResp(c, err, "permission denied")
		return
	}

	srcRegexp, err := regexp.Compile(req.SrcNameRegex)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	files, err := fs.List(c, reqPath, &fs.ListArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	files = op.FilterListing(c, user, reqPath, files)

	for _, file := range files {

		if srcRegexp.MatchString(file.GetName()) {
			filePath, err := utils.JoinUnderBase(reqPath, file.GetName())
			if err != nil {
				common.ErrorResp(c, err, 500)
				return
			}
			if !canRenamePath(c, filePath) {
				return
			}
			// FilterListing above already drops children with no ActRead
			// (the common case for a "fully denied" subtree), but a deny
			// scoped to ActRename only (read still allowed) would stay
			// visible in files and reach here undetected without this
			// per-target check.
			if err := op.Authorize(c, user, model.ActRename, op.AuthzRequest{Path: filePath}); err != nil {
				common.AuthzErrorResp(c, err, "permission denied")
				return
			}
			newFileName := srcRegexp.ReplaceAllString(file.GetName(), req.NewNameRegex)
			if err := utils.ValidateNameComponent(newFileName); err != nil {
				common.ErrorResp(c, err, 400)
				return
			}
			if err := fs.Rename(c, filePath, newFileName); err != nil {
				common.ErrorResp(c, err, 500)
				return
			}
		}

	}

	common.SuccessResp(c)
}
