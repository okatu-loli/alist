package handles

import (
	"bytes"
	"fmt"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"io"
	stdpath "path"
	"strconv"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	"github.com/microcosm-cc/bluemonday"
	log "github.com/sirupsen/logrus"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// Down resolves a signed direct-download link for a path and redirects to it
// (or falls back to Proxy when the storage requires it).
// @Summary Download (redirect)
// @Description Resolves the storage's direct link for the signed path and issues a 302 redirect to it; proxies through the server instead when the storage requires it (WebProxy/must-proxy/proxy_types, etc). Mounted at the top-level route (not under /api) as GET and HEAD /d/{path}; the path must carry a valid `sign` query parameter (see middlewares.Down) rather than an Authorization header.
// @Tags download
// @Produce json
// @Param path path string true "storage-relative file path"
// @Param sign query string true "path signature, see /admin/signing_key"
// @Param type query string false "link type hint passed through to the driver"
// @Success 302 {string} string "redirect to the resolved direct link"
// @Failure 500 {object} common.Resp[any]
// @Router /../d/{path} [get]
func Down(c *gin.Context) {
	rawPath := c.MustGet(ctxkey.Path).(string)
	filename := stdpath.Base(rawPath)
	storage, err := fs.GetStorage(rawPath, &fs.GetStoragesArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if common.ShouldProxy(storage, filename) {
		Proxy(c)
		return
	} else {
		link, _, err := fs.Link(c, rawPath, model.LinkArgs{
			IP:       c.ClientIP(),
			Header:   c.Request.Header,
			Type:     c.Query("type"),
			HttpReq:  c.Request,
			Redirect: true,
		})
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		common.CompleteLink(c.Request, link)
		down(c, link)
	}
}

// Proxy streams a file's content through the server rather than redirecting to
// the storage's direct link (used when the storage forbids direct links, or the
// caller opted into proxying via /p/{path}).
// @Summary Download (proxied through server)
// @Description Streams the file content through this server instead of redirecting. Markdown files are additionally rendered to sanitized HTML when the filter-readme-scripts setting is on. Mounted at the top-level route (not under /api) as GET and HEAD /p/{path}; the path must carry a valid `sign` query parameter (see middlewares.Down) rather than an Authorization header.
// @Tags download
// @Produce octet-stream
// @Param path path string true "storage-relative file path"
// @Param sign query string true "path signature, see /admin/signing_key"
// @Param type query string false "link type hint passed through to the driver"
// @Param d query string false "presence disables redirect to the storage's down_proxy_url"
// @Success 200 {file} file "file content"
// @Failure 403 {object} common.Resp[any] "proxy not allowed for this storage/file type"
// @Failure 500 {object} common.Resp[any]
// @Router /../p/{path} [get]
func Proxy(c *gin.Context) {
	rawPath := c.MustGet(ctxkey.Path).(string)
	filename := stdpath.Base(rawPath)
	storage, err := fs.GetStorage(rawPath, &fs.GetStoragesArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if c.Query("type") == "preview" && storage.GetStorage().Driver == "DoubaoNew" {
		// Force proxy for DoubaoNew preview so headers are preserved.
		link, file, err := fs.Link(c, rawPath, model.LinkArgs{
			Header:  c.Request.Header,
			Type:    c.Query("type"),
			HttpReq: c.Request,
		})
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		localProxy(c, link, file, storage.GetStorage().ProxyRange)
		return
	}
	if canProxy(storage, filename) {
		downProxyUrl := storage.GetStorage().DownProxyUrl
		if downProxyUrl != "" {
			_, ok := c.GetQuery("d")
			if !ok {
				URL := common.BuildDownProxyURL(downProxyUrl, rawPath, storage.GetStorage().DownProxySign)
				c.Redirect(302, URL)
				return
			}
		}
		link, file, err := fs.Link(c, rawPath, model.LinkArgs{
			Header:  c.Request.Header,
			Type:    c.Query("type"),
			HttpReq: c.Request,
		})
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		localProxy(c, link, file, storage.GetStorage().ProxyRange)
	} else {
		common.ErrorI18n(c, "down.proxy_not_allowed", 403)
		return
	}
}

func down(c *gin.Context, link *model.Link) {
	var err error
	if link.MFile != nil {
		defer func(ReadSeekCloser io.ReadCloser) {
			err := ReadSeekCloser.Close()
			if err != nil {
				log.Errorf("close data error: %s", err)
			}
		}(link.MFile)
	}
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Cache-Control", "max-age=0, no-cache, no-store, must-revalidate")
	if setting.GetBool(conf.ForwardDirectLinkParams) {
		query := c.Request.URL.Query()
		for _, v := range conf.SlicesMap[conf.IgnoreDirectLinkParams] {
			query.Del(v)
		}
		link.URL, err = utils.InjectQuery(link.URL, query)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
	}
	c.Redirect(302, link.URL)
}

func localProxy(c *gin.Context, link *model.Link, file model.Obj, proxyRange bool) {
	var err error
	if link.URL != "" && setting.GetBool(conf.ForwardDirectLinkParams) {
		query := c.Request.URL.Query()
		for _, v := range conf.SlicesMap[conf.IgnoreDirectLinkParams] {
			query.Del(v)
		}
		link.URL, err = utils.InjectQuery(link.URL, query)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
	}
	if proxyRange {
		common.ProxyRange(link, file.GetSize())
	}
	Writer := &common.WrittenResponseWriter{ResponseWriter: c.Writer}

	//优先处理md文件
	if utils.Ext(file.GetName()) == "md" && setting.GetBool(conf.FilterReadMeScripts) {
		buf := bytes.NewBuffer(make([]byte, 0, file.GetSize()))
		w := &common.InterceptResponseWriter{ResponseWriter: Writer, Writer: buf}
		err = common.Proxy(w, c.Request, link, file)
		if err == nil && buf.Len() > 0 {
			if c.Writer.Status() < 200 || c.Writer.Status() > 300 {
				c.Writer.Write(buf.Bytes())
				return
			}

			var html bytes.Buffer
			md := goldmark.New(goldmark.WithExtensions(extension.GFM))
			if err = md.Convert(buf.Bytes(), &html); err != nil {
				err = fmt.Errorf("markdown conversion failed: %w", err)
			} else {
				buf.Reset()
				err = bluemonday.UGCPolicy().SanitizeReaderToWriter(&html, buf)
				if err == nil {
					Writer.Header().Set("Content-Length", strconv.FormatInt(int64(buf.Len()), 10))
					Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
					_, err = utils.CopyWithBuffer(Writer, buf)
				}
			}
		}
	} else {
		err = common.Proxy(Writer, c.Request, link, file)
	}
	if err == nil {
		return
	}
	if Writer.IsWritten() {
		log.Errorf("%s %s local proxy error: %+v", c.Request.Method, c.Request.URL.Path, err)
	} else {
		common.ErrorResp(c, err, 500, true)
	}
}

// TODO need optimize
// when can be proxy?
// 1. text file
// 2. config.MustProxy()
// 3. storage.WebProxy
// 4. proxy_types
// solution: text_file + shouldProxy()
func canProxy(storage driver.Driver, filename string) bool {
	if storage.Config().MustProxy() || storage.GetStorage().WebProxy || storage.GetStorage().WebdavProxy() {
		return true
	}
	if storage.GetStorage().Driver == "Quark" && utils.GetFileType(filename) == conf.VIDEO {
		return true
	}
	if utils.SliceContains(conf.SlicesMap[conf.ProxyTypes], utils.Ext(filename)) {
		return true
	}
	if utils.SliceContains(conf.SlicesMap[conf.TextTypes], utils.Ext(filename)) {
		return true
	}
	return false
}
