package middlewares

import (
	"strings"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/internal/sign"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// Down verifies the download sign against the ResourceRef canonical form of
// the requested path — the SAME path→ref resolution common.SignPath uses to
// mint it — rather than the raw path string, so a mount rename does not
// invalidate outstanding links. It takes no verify function: unlike the old
// sign.Verify(rawPath, sign) call, resolving the ref is now part of
// verification itself, not something a caller can swap in from outside.
func Down() func(c *gin.Context) {
	return func(c *gin.Context) {
		rawPath := parsePath(c.Param("path"))
		c.Set(ctxkey.Path, rawPath)
		meta, err := op.GetNearestMeta(rawPath)
		if err != nil {
			if !errors.Is(errors.Cause(err), errs.MetaNotFound) {
				common.ErrorResp(c, err, 500, true)
				return
			}
		}
		// verify sign
		if needSign(meta, rawPath) {
			s := c.Query("sign")
			// Same fallback as common.SignPath: a raw path above every mount
			// point has no ref to anchor to, so it verifies against the raw
			// path string sign.Sign fell back to signing.
			canonical := rawPath
			if ref, err := op.ResolvePathAnchor(rawPath); err == nil {
				canonical = ref.Canonical()
			}
			if err := sign.Verify(canonical, strings.TrimSuffix(s, "/")); err != nil {
				common.ErrorResp(c, err, 401)
				c.Abort()
				return
			}
		}
		c.Next()
	}
}

func parsePath(path string) string {
	// gin already unescapes path params (UnescapePathValues defaults to true),
	// so `path` is decoded exactly once here. A second url.PathUnescape would
	// double-decode names containing a literal '%': e.g. a file named
	// "%7PFD7BW{)…png" arrives URL-encoded as "%257PFD7BW%7B%29…", gin decodes it
	// to "%7PFD7BW{)…", and a second unescape then chokes on the now-bare "%7P"
	// (invalid escape) — corrupting the path so its ResourceRef canonical no
	// longer matches the one the link was signed against ("sign invalid").
	return utils.FixAndCleanPath(path)
}

func needSign(meta *model.Meta, path string) bool {
	if setting.GetBool(conf.SignAll) {
		return true
	}
	if common.IsStorageSignEnabled(path) {
		return true
	}
	if meta == nil || meta.Password == "" {
		return false
	}
	if !meta.PSub && path != meta.Path {
		return false
	}
	return true
}
