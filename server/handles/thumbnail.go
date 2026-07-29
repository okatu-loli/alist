package handles

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/thumbnail"
	"github.com/gin-gonic/gin"
)

// thumbCacheMaxAgeSeconds is the browser Cache-Control lifetime for a served
// thumbnail. The cache key already folds in the object's ModTime (see
// internal/thumbnail), so a stale cached image is impossible even with a long
// browser-side TTL -- a content change mints a new ETag/URL-equivalent key,
// it never reuses this one.
const thumbCacheMaxAgeSeconds = 86400 // 1 day

// FsThumb returns a resized JPEG thumbnail for an image file the caller may
// read.
// @Summary Get file thumbnail
// @Description Returns a resized JPEG preview of an image file (jpg/jpeg/png/gif/webp/bmp/tiff only -- video is out of scope). Generated on first request and cached under the data dir, keyed by path+size+modtime, so a stale cache is never served. op.Authorize(ActRead) on path is the sole access boundary: a caller only ever thumbnails what they could otherwise read.
// @Tags fs
// @Security ApiKeyAuth
// @Produce image/jpeg
// @Param path query string true "file path"
// @Param w query int false "max dimension in pixels, clamped to 32-512 (default 256)"
// @Success 200 {file} binary "JPEG thumbnail"
// @Failure 304 "not modified (If-None-Match matched the current ETag)"
// @Failure 400 "missing path"
// @Failure 403 "no read permission on path"
// @Failure 404 "not found, a directory, or not a thumbnailable image"
// @Failure 422 "source too large or not a decodable image"
// @Failure 500 "internal error"
// @Router /v4/fs/thumb [get]
func FsThumb(c *gin.Context) {
	// Real HTTP statuses (not the 200+JSON envelope every other fs.* handler
	// uses): this is a browser <img src> subresource load, same reasoning as
	// PluginAsset (see extension.go) -- a 404 must be an actual 404 so the
	// <img> tag's onerror/broken-image behavior fires, and a hit must be the
	// raw JPEG bytes with its own content-type, not a JSON envelope.
	rawPath := c.Query("path")
	if rawPath == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	maxDim := thumbnail.DefaultMaxDim
	if ws := c.Query("w"); ws != "" {
		if v, err := strconv.Atoi(ws); err == nil {
			maxDim = v
		}
		// A non-numeric w= is silently treated as "unset" (falls back to the
		// default) rather than a 400 -- a thumbnail request is best-effort
		// resource loading, not a strict API call.
	}

	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(rawPath)
	if err != nil {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	data, cachedPath, err := thumbnail.Generate(c, user, reqPath, maxDim)
	if err != nil {
		c.AbortWithStatus(thumbErrStatus(err))
		return
	}

	// The cache file name is already path+size+modtime, content-addressed and
	// unique per generated thumbnail -- reused directly as the ETag validator,
	// no extra hashing needed.
	etag := `"` + filepath.Base(cachedPath) + `"`
	c.Header("ETag", etag)
	c.Header("Cache-Control", fmt.Sprintf("private, max-age=%d", thumbCacheMaxAgeSeconds))
	if inm := c.GetHeader("If-None-Match"); inm != "" && inm == etag {
		// c.Status alone only records the code on gin's buffered
		// responseWriter; nothing here calls Write (which is what normally
		// flushes it), so WriteHeaderNow is required to actually send a
		// (bodyless) 304 rather than gin defaulting to 200 once the handler
		// returns having written nothing.
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		return
	}
	c.Data(http.StatusOK, "image/jpeg", data)
}

// thumbErrStatus maps a thumbnail.Generate error to an HTTP status:
// unauthorized -> 403, not-found/not-a-thumbnailable-image -> 404,
// oversize/undecodable source -> 422 (the source exists and is readable, but
// the server correctly refuses to process it), anything else -> 500.
func thumbErrStatus(err error) int {
	switch {
	case errors.Is(err, errs.PermissionDenied), errors.Is(err, errs.WrongPassword):
		return http.StatusForbidden
	case errors.Is(err, thumbnail.ErrNotImageThumbnailable),
		errs.IsObjectNotFound(err),
		errs.IsNotFoundError(err):
		return http.StatusNotFound
	case errors.Is(err, thumbnail.ErrSourceTooLarge),
		errors.Is(err, thumbnail.ErrDecodeFailed),
		errors.Is(err, thumbnail.ErrRenderedThumbnailTooLarge):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
