package handles

import (
	"errors"
	"net/url"
	stdpath "path"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// Public, UNAUTHENTICATED share access (design §2.3-B / §4). This is the security
// core of the feature: it hands the creator's read right to anyone holding the
// token, so it deliberately bypasses op.Authorize (the creator already proved
// ActRead at mint time) and instead relies on three gates, in order, on every
// request:
//
//  1. op.ResolveShare — token → live share. Missing / expired / disabled all
//     collapse to one 404 (enumeration defense: a probe can't tell which).
//  2. sharePasswordOK — a password-protected share yields NOTHING (no list, no
//     download, no content) until a constant-time password check passes (403).
//  3. op.ShareSubPath — THE confinement gate. Every client-supplied ?sub= is
//     joined onto the share root and must resolve inside the shared subtree; any
//     traversal (../, absolute, backslash, encoded) is refused. A path outside
//     the subtree is never listed and never signed into a /d link.
//
// These handlers are mounted OUTSIDE the auth group (server/router_v4.go), so
// they run with no user in context and never read Authorization/Cookie.

// errSharePassword is the 403 returned when a password-protected share is
// accessed without the correct password. It is deliberately generic — it reveals
// only that a password is required/incorrect, nothing about the share contents.
var errSharePassword = errors.New("a valid share password is required")

// sharePasswordOK reports whether the request satisfies the share's password
// gate. A share with no password is always open; otherwise the ?password= query
// is checked against the stored salted hash in constant time (utils.VerifyToken).
func sharePasswordOK(s *model.Share, c *gin.Context) bool {
	if s.PasswordHash == "" {
		return true
	}
	return utils.VerifyToken(s.PasswordHash, c.Query("password"))
}

// ShareInfo (GET /s/:token) returns the minimal public meta a landing page needs:
// the share-root base name, whether it is a directory, whether a password is
// required, and the expiry. When a password IS required it still returns only
// this meta — never a listing or content. The full internal virtual path, the
// creator and the PasswordHash are never exposed.
// @Summary Get share info
// @Description Returns the minimal public meta a landing page needs for a share token: base name, whether it is a directory, whether a password is required, and expiry. Anonymous, unauthenticated — no Authorization header is read. Missing/expired/disabled tokens all collapse to a uniform 404 (enumeration defense).
// @Tags shares
// @Produce json
// @Param token path string true "share token"
// @Success 200 {object} common.Resp[any] "name, is_dir, needs_password, expires_at"
// @Failure 404 {object} common.Resp[any] "share not found, expired, or disabled"
// @Router /v4/s/{token} [get]
func ShareInfo(c *gin.Context) {
	s, root, err := op.ResolveShare(c.Param("token"))
	if err != nil {
		// Uniform 404 for missing/expired/disabled (enumeration defense).
		common.ErrorResp(c, errs.ShareNotFound, 404)
		return
	}
	obj, err := fs.Get(c, root, &fs.GetArgs{NoLog: true})
	if err != nil {
		common.ErrorResp(c, errs.ShareNotFound, 404)
		return
	}
	common.SuccessResp(c, gin.H{
		"name":           stdpath.Base(root),
		"is_dir":         obj.IsDir(),
		"needs_password": s.PasswordHash != "",
		"expires_at":     s.ExpiresAt,
	})
}

// ShareList (GET /s/:token/list?sub=&password=) lists the direct children of a
// path within the shared subtree. The subtree is confined by op.ShareSubPath, so
// ?sub= can never walk out of the share. Each item exposes only {name,is_dir,
// size} — never the internal virtual path.
// @Summary List a share's contents
// @Description Lists the direct children of a path within the shared subtree. ?sub= is confined to the subtree by op.ShareSubPath; any escape attempt is folded into the same 404 as a missing share. Anonymous, unauthenticated.
// @Tags shares
// @Produce json
// @Param token path string true "share token"
// @Param sub query string false "sub-path within the share root"
// @Param password query string false "share password, required when the share is password-protected"
// @Success 200 {object} common.Resp[any] "list of {name, is_dir, size}"
// @Failure 403 {object} common.Resp[any] "password required or incorrect"
// @Failure 404 {object} common.Resp[any] "share not found, expired, disabled, or sub-path out of scope"
// @Router /v4/s/{token}/list [get]
func ShareList(c *gin.Context) {
	s, root, err := op.ResolveShare(c.Param("token"))
	if err != nil {
		common.ErrorResp(c, errs.ShareNotFound, 404)
		return
	}
	if !sharePasswordOK(s, c) {
		common.ErrorResp(c, errSharePassword, 403)
		return
	}
	full, err := op.ShareSubPath(root, c.Query("sub"))
	if err != nil {
		// Out-of-scope: fold into the same 404 so it leaks neither the escape nor
		// any foreign path.
		common.ErrorResp(c, errs.ShareNotFound, 404)
		return
	}
	objs, err := fs.List(c, full, &fs.ListArgs{NoLog: true})
	if err != nil {
		// Never surface the underlying fs error (it can carry the internal path);
		// a bad/absent target inside the share is just a 404 to the public caller.
		common.ErrorResp(c, errs.ShareNotFound, 404)
		return
	}
	items := make([]gin.H, 0, len(objs))
	for _, o := range objs {
		items = append(items, gin.H{
			"name":   o.GetName(),
			"is_dir": o.IsDir(),
			"size":   o.GetSize(),
		})
	}
	common.SuccessResp(c, items)
}

// ShareDownload (GET /s/:token/download?sub=&password=) 302-redirects a file
// within the shared subtree to the existing unauthenticated /d sign channel. The
// path is confined by op.ShareSubPath BEFORE it is signed, so a signature is only
// ever minted for an in-subtree file — never a path the share did not grant. The
// Location is built via net/url so names with spaces/brackets/CJK are correctly
// percent-encoded (never hand-concatenated).
// @Summary Download a shared file (redirect)
// @Description 302-redirects a file within the shared subtree to the existing unauthenticated /d sign channel. The path is confined by op.ShareSubPath before it is signed, so a signature is only ever minted for an in-subtree file. Anonymous, unauthenticated.
// @Tags shares
// @Produce json
// @Param token path string true "share token"
// @Param sub query string false "sub-path within the share root, must resolve to a file"
// @Param password query string false "share password, required when the share is password-protected"
// @Success 302 {string} string "redirect to the signed /d link"
// @Failure 400 {object} common.Resp[any] "sub-path resolves to a directory"
// @Failure 403 {object} common.Resp[any] "password required or incorrect"
// @Failure 404 {object} common.Resp[any] "share not found, expired, disabled, or sub-path out of scope"
// @Router /v4/s/{token}/download [get]
func ShareDownload(c *gin.Context) {
	s, root, err := op.ResolveShare(c.Param("token"))
	if err != nil {
		common.ErrorResp(c, errs.ShareNotFound, 404)
		return
	}
	if !sharePasswordOK(s, c) {
		common.ErrorResp(c, errSharePassword, 403)
		return
	}
	full, err := op.ShareSubPath(root, c.Query("sub"))
	if err != nil {
		common.ErrorResp(c, errs.ShareNotFound, 404)
		return
	}
	obj, err := fs.Get(c, full, &fs.GetArgs{NoLog: true})
	if err != nil {
		common.ErrorResp(c, errs.ShareNotFound, 404)
		return
	}
	if obj.IsDir() {
		common.ErrorResp(c, errors.New("is a directory"), 400)
		return
	}
	// full is already confirmed in-subtree; sign exactly it, nothing broader.
	signed := common.SignPath(full)
	loc := (&url.URL{Path: "/d" + full, RawQuery: "sign=" + url.QueryEscape(signed)}).String()
	c.Redirect(302, loc)
}
