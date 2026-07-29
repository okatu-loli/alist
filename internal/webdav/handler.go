package webdav

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	xwebdav "golang.org/x/net/webdav"
)

// Prefix is the URL path prefix the WebDAV handler is mounted at: a
// top-level route outside /api/v4 (design W5 §④ -- "WebDAV 客户端要干净 URL
// 根"). server/router.go mounts "/dav/*path" behind this same prefix.
const Prefix = "/dav"

// davHandler is the stock xwebdav.Handler doing all RFC 4918 protocol work
// (PROPFIND XML, locking, etc). alistFS is stateless (see fs.go), and
// xwebdav.NewMemLS() is an in-process lock table -- fine for a single
// instance, explicitly YAGNI'd for multi-instance sharing (design
// §非目标).
var davHandler = &xwebdav.Handler{
	Prefix:     Prefix,
	FileSystem: alistFS{},
	LockSystem: xwebdav.NewMemLS(),
}

// deniedFlagKey is the request-scoped context key Handler stashes a *bool
// under; fs.go's denyErr sets it to true the moment any op.Authorize call
// fails. It is unexported and non-string on purpose: unlike ctxkey.User it
// must never collide with (or be settable via) gin.Context.Set.
type deniedFlagKey struct{}

func markDenied(ctx context.Context) {
	if f, ok := ctx.Value(deniedFlagKey{}).(*bool); ok {
		*f = true
	}
}

// deniedInterceptWriter forces the final HTTP status to 403 whenever the
// request's deniedFlagKey was set, regardless of what xwebdav.Handler itself
// picked.
//
// This exists because golang.org/x/net/webdav's own status mapping never
// produces 403: handlePut/handleMkcol map a non-IsNotExist FileSystem error
// to 404/405, handleDelete to 405, handleGetHeadPost always to 404. Design
// W5 §② requires "只读用户(无写 scope)PUT/MKCOL/DELETE 被 op.Authorize 拒 →
// 映射 403", which the stock library cannot produce on its own -- this
// wrapper is the seam that makes it true without forking the handler's XML
// dispatch logic. It only ever raises severity (never turns a real error
// into success): the flag is set exclusively on an op.Authorize denial, and
// every such path already returns a status >= 400 from the library.
type deniedInterceptWriter struct {
	http.ResponseWriter
	denied *bool
	wrote  bool
}

func (w *deniedInterceptWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	if *w.denied && status >= http.StatusBadRequest {
		status = http.StatusForbidden
	}
	w.ResponseWriter.WriteHeader(status)
}

// Handler serves one WebDAV request. It must run after the builtin.webdav
// extension gate and BasicAuth (server/router.go), so ctxkey.User is already
// on the gin context by the time the FileSystem adapter looks for it.
func Handler(c *gin.Context) {
	denied := new(bool)
	// c (*gin.Context) implements context.Context itself, resolving string
	// keys via c.Get -- wrapping it with context.WithValue layers the
	// non-string deniedFlagKey on top while every ctxkey.* / audit.Ctx*
	// lookup still falls through to c.Value exactly as it does everywhere
	// else in the codebase.
	ctx := context.WithValue(c, deniedFlagKey{}, denied)
	r := c.Request.WithContext(ctx)
	iw := &deniedInterceptWriter{ResponseWriter: c.Writer, denied: denied}
	davHandler.ServeHTTP(iw, r)
}
