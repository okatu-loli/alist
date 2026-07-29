package handles

import (
	"errors"
	"mime"
	"net/http"
	stdpath "path"

	"github.com/alist-org/alist/v3/internal/archive"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// Archive browsing (internal/archive): let a caller look inside an archive
// file they can already read, without downloading the whole thing first.
// Format support is dispatched by file extension against every registered
// Kind=ArchiveTool (the built-in zip tool, plus any installed plugin adding
// rar/7z/...) -- see internal/extension.FindArchiveToolForExt; a path whose
// extension nothing claims behaves like any other unsupported format.
// op.Authorize(ActRead) on the archive's own path (enforced inside
// internal/archive) is the sole access boundary, mirroring every other fs.*
// handler and internal/thumbnail. Meta/List return the usual 200+JSON
// envelope (common.SuccessResp/ErrorResp -- the JSON `code` field carries the
// logical status, same convention as FsList/FsGet); Extract streams the
// entry's bytes with a REAL HTTP status, same reasoning as FsThumb (a
// download, not a JSON API call).

type ArchiveMetaReq struct {
	Path     string `json:"path" form:"path"`
	Password string `json:"password" form:"password"`
}

// FsArchiveMeta returns a zip archive's meta-info (entry count, whether any
// entry is encrypted, total declared uncompressed size).
// @Summary Get zip archive meta-info
// @Description Dispatched by file extension to a registered ArchiveTool (built-in zip, or an installed plugin). op.Authorize(ActRead) on path is the sole access boundary.
// @Tags fs
// @Security ApiKeyAuth
// @Produce json
// @Param path query string true "archive file path"
// @Param password query string false "archive password (reserved -- unused for std zip, which cannot decrypt)"
// @Success 200 {object} common.Resp[archive.ArchiveMeta]
// @Failure 400 {object} common.Resp[any] "missing path"
// @Failure 403 {object} common.Resp[any] "no read permission on path"
// @Failure 404 {object} common.Resp[any] "not found or not a zip archive"
// @Failure 422 {object} common.Resp[any] "zip exceeds a safety cap (entry count / total size)"
// @Router /v4/fs/archive/meta [get]
func FsArchiveMeta(c *gin.Context) {
	var req ArchiveMetaReq
	if err := c.ShouldBindQuery(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if req.Path == "" {
		common.ErrorResp(c, errors.New("path is required"), 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	meta, err := archive.Meta(c, user, reqPath, req.Password)
	if err != nil {
		archiveErrorResp(c, err)
		return
	}
	common.SuccessResp(c, meta)
}

type ArchiveListReq struct {
	Path     string `json:"path" form:"path"`
	Inner    string `json:"inner" form:"inner"`
	Password string `json:"password" form:"password"`
}

// FsArchiveList lists the immediate children of Inner within the zip at
// Path (innerPath="" is the archive root).
// @Summary List a zip archive's inner directory
// @Description Dispatched by file extension to a registered ArchiveTool (built-in zip, or an installed plugin). Virtual-directory view: returns direct children of `inner` only, not the whole flat entry list. op.Authorize(ActRead) on path is the sole access boundary.
// @Tags fs
// @Security ApiKeyAuth
// @Produce json
// @Param path query string true "archive file path"
// @Param inner query string false "inner directory path within the archive (root if empty)"
// @Param password query string false "archive password (reserved -- unused for std zip)"
// @Success 200 {object} common.Resp[[]archive.Entry]
// @Failure 400 {object} common.Resp[any] "missing path"
// @Failure 403 {object} common.Resp[any] "no read permission on path"
// @Failure 404 {object} common.Resp[any] "not found, not a zip archive, or inner path not found"
// @Failure 422 {object} common.Resp[any] "zip exceeds a safety cap"
// @Router /v4/fs/archive/list [get]
func FsArchiveList(c *gin.Context) {
	var req ArchiveListReq
	if err := c.ShouldBindQuery(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if req.Path == "" {
		common.ErrorResp(c, errors.New("path is required"), 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	entries, err := archive.List(c, user, reqPath, req.Inner, req.Password)
	if err != nil {
		archiveErrorResp(c, err)
		return
	}
	common.SuccessResp(c, entries)
}

// FsArchiveExtract streams one inner file entry's bytes. Real HTTP statuses
// (not the 200+JSON envelope), same reasoning as FsThumb: this is a
// browser-navigable download/preview URL, not a JSON API call.
// @Summary Extract one file from a zip archive
// @Description Dispatched by file extension to a registered ArchiveTool (built-in zip, or an installed plugin). Streams the inner file's bytes directly (Content-Disposition: inline). A single entry's declared uncompressed size is capped; the entry itself is streamed, never buffered whole.
// @Tags fs
// @Security ApiKeyAuth
// @Param path query string true "archive file path"
// @Param inner query string true "inner file path within the archive"
// @Param password query string false "archive password (reserved -- unused for std zip)"
// @Success 200 {file} binary "the inner file's raw bytes"
// @Failure 400 {object} common.Resp[any] "missing path/inner, or inner path is a directory"
// @Failure 403 {object} common.Resp[any] "no read permission on path"
// @Failure 404 {object} common.Resp[any] "not found, not a zip archive, or inner path not found"
// @Failure 422 {object} common.Resp[any] "encrypted entry, or exceeds a safety cap"
// @Router /v4/fs/archive/extract [get]
func FsArchiveExtract(c *gin.Context) {
	rawPath := c.Query("path")
	if rawPath == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	inner := c.Query("inner")
	password := c.Query("password")

	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(rawPath)
	if err != nil {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	rc, size, err := archive.Extract(c, user, reqPath, inner, password)
	if err != nil {
		c.AbortWithStatus(archiveErrStatus(err))
		return
	}
	defer rc.Close()

	name := stdpath.Base(inner)
	ct := mime.TypeByExtension(stdpath.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	c.Header("Content-Disposition", `inline; filename="`+escapeContentDispositionFilename(name)+`"`)
	c.DataFromReader(http.StatusOK, size, ct, rc, nil)
}

// escapeContentDispositionFilename prevents a filename containing a double
// quote (or backslash) from breaking out of the Content-Disposition header's
// quoted-string.
func escapeContentDispositionFilename(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// archiveErrorResp maps an internal/archive error to the 200+JSON envelope's
// logical code, for the JSON-returning Meta/List handlers.
func archiveErrorResp(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errs.PermissionDenied), errors.Is(err, errs.WrongPassword):
		common.AuthzErrorResp(c, err, "password is incorrect or you have no permission")
	case errors.Is(err, archive.ErrNotArchive),
		errors.Is(err, archive.ErrInnerNotFound),
		errs.IsObjectNotFound(err),
		errs.IsNotFoundError(err):
		common.ErrorResp(c, err, 404)
	case errors.Is(err, archive.ErrArchiveEncrypted),
		errors.Is(err, archive.ErrTooManyEntries),
		errors.Is(err, archive.ErrArchiveTooLarge),
		errors.Is(err, archive.ErrEntryTooLarge):
		common.ErrorResp(c, err, 422)
	default:
		common.ErrorResp(c, err, 500, true)
	}
}

// archiveErrStatus maps an internal/archive error to a REAL HTTP status, for
// the byte-streaming Extract handler.
func archiveErrStatus(err error) int {
	switch {
	case errors.Is(err, errs.PermissionDenied), errors.Is(err, errs.WrongPassword):
		return http.StatusForbidden
	case errors.Is(err, archive.ErrNotArchive),
		errors.Is(err, archive.ErrInnerNotFound),
		errs.IsObjectNotFound(err),
		errs.IsNotFoundError(err):
		return http.StatusNotFound
	case errors.Is(err, archive.ErrIsDir):
		return http.StatusBadRequest
	case errors.Is(err, archive.ErrArchiveEncrypted),
		errors.Is(err, archive.ErrTooManyEntries),
		errors.Is(err, archive.ErrArchiveTooLarge),
		errors.Is(err, archive.ErrEntryTooLarge):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
