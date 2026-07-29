package handles

import (
	"errors"
	"fmt"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	stdpath "path"
	"strings"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	pkgerr "github.com/pkg/errors"
)

type ListReq struct {
	model.PageReq
	Path     string `json:"path" form:"path"`
	Password string `json:"password" form:"password"`
	Refresh  bool   `json:"refresh" form:"refresh"`
	// Fields requests extension field groups (comma-separated, "_"-prefixed, e.g.
	// "_labels,_shares") for the listed page (design §2.6). Base fields always
	// return; a requested group is computed on demand by its FileDecorator under a
	// per-group budget, and a group that exceeds the budget is returned in Pending
	// for the client to backfill via /fs/fields. Empty means no decoration.
	Fields string `json:"fields" form:"fields"`
}

type DirReq struct {
	Path      string `json:"path" form:"path"`
	Password  string `json:"password" form:"password"`
	ForceRoot bool   `json:"force_root" form:"force_root"`
}

type ObjResp struct {
	Id           string                     `json:"id"`
	Path         string                     `json:"path"`
	VirtualPath  string                     `json:"virtual_path"`
	Name         string                     `json:"name"`
	Size         int64                      `json:"size"`
	IsDir        bool                       `json:"is_dir"`
	Modified     time.Time                  `json:"modified"`
	Created      time.Time                  `json:"created"`
	Sign         string                     `json:"sign"`
	Thumb        string                     `json:"thumb"`
	Type         int                        `json:"type"`
	HashInfoStr  string                     `json:"hashinfo"`
	HashInfo     map[*utils.HashType]string `json:"hash_info"`
	StorageClass string                     `json:"storage_class,omitempty"`
}

type FsListResp struct {
	Content       []ObjListResp `json:"content"`
	Total         int64         `json:"total"`
	FilteredTotal int64         `json:"filtered_total"`
	Page          int           `json:"page"`
	PerPage       int           `json:"per_page"`
	HasMore       bool          `json:"has_more"`
	PagesTotal    int           `json:"pages_total"`
	Readme        string        `json:"readme"`
	Header        string        `json:"header"`
	Write         bool          `json:"write"`
	Provider      string        `json:"provider"`
	// Fields/Pending carry extension field-group decorations for the listed page
	// (design §2.6), present only when the request asked for fields. Fields maps
	// group → ref key (ext.RefKey, the object's virtual path) → column value;
	// Pending lists the groups whose decorator exceeded its budget, to be
	// backfilled via /fs/fields.
	Fields  map[string]map[string]any `json:"fields,omitempty"`
	Pending []string                  `json:"pending,omitempty"`
}

type ObjListResp struct {
	Id           string                     `json:"id"`
	Path         string                     `json:"path"`
	VirtualPath  string                     `json:"virtual_path"`
	Name         string                     `json:"name"`
	Size         int64                      `json:"size"`
	IsDir        bool                       `json:"is_dir"`
	Modified     time.Time                  `json:"modified"`
	Created      time.Time                  `json:"created"`
	Sign         string                     `json:"sign"`
	Thumb        string                     `json:"thumb"`
	Type         int                        `json:"type"`
	HashInfoStr  string                     `json:"hashinfo"`
	HashInfo     map[*utils.HashType]string `json:"hash_info"`
	StorageClass string                     `json:"storage_class,omitempty"`
}

const (
	DefaultPerPage = 200
	MaxPerPage     = 500
	AllPerPage     = -1
)

// FsList lists the contents of a directory.
// @Summary List directory
// @Description Registered for any HTTP method via gin Any(). Returns paginated directory contents, honoring per-storage/meta permissions.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body ListReq true "path/password/pagination/refresh"
// @Success 200 {object} common.Resp[FsListResp]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "password incorrect or no permission"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/list [post]
// @Router /v4/fs/list [get]
func FsList(c *gin.Context) {
	var req ListReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	effPage, effPerPage := normalizeListPage(req.Page, req.PerPage)
	req.Page = effPage
	req.PerPage = effPerPage
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: reqPath, Password: req.Password}); err != nil {
		common.AuthzErrorResp(c, err, "password is incorrect or you have no permission")
		return
	}
	em, err := op.ResolveEffectiveMeta(reqPath)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	canWrite := op.Authorize(c, user, model.ActWrite, op.AuthzRequest{Path: reqPath}) == nil
	if req.Refresh && !canWrite {
		common.ErrorI18n(c, "fsread.refresh_without_permission", 403)
		return
	}
	provider := "unknown"
	storage, storageErr := fs.GetStorage(reqPath, &fs.GetStoragesArgs{})
	if storageErr == nil {
		provider = storage.GetStorage().Driver
	}
	objs, err := fs.List(c, reqPath, &fs.ListArgs{Refresh: req.Refresh})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	filtered := op.FilterListing(c, user, reqPath, objs)
	total, pageObjs := pagination(filtered, &req.PageReq)
	respContent := toObjsResp(pageObjs, reqPath, isEncrypt(reqPath))
	pagesTotal := calcPagesTotal(total, req.PerPage)
	hasMore := req.PerPage != AllPerPage && req.Page*req.PerPage < total

	resp := FsListResp{
		Content:       respContent,
		Total:         int64(total),
		FilteredTotal: int64(total),
		Page:          req.Page,
		PerPage:       req.PerPage,
		HasMore:       hasMore,
		PagesTotal:    pagesTotal,
		Readme:        em.Readme,
		Header:        em.Header,
		Write:         canWrite,
		Provider:      provider,
	}
	// Extension field groups (design §2.6): only the page's already-filtered
	// (user-visible) objects are handed to decorators, and only when the client
	// asked for a group. A slow decorator is returned in Pending, never blocking
	// the base listing beyond the per-group budget.
	if groups := splitFields(req.Fields); len(groups) > 0 {
		refs := make([]ext.ObjRef, 0, len(pageObjs))
		for _, o := range pageObjs {
			refs = append(refs, ext.ObjRef{Anchor: stdpath.Join(reqPath, o.GetName())})
		}
		fr := extension.DecorateFields(c, extension.DefaultRegistry, groups, refs, 0)
		resp.Fields = fr.Fields
		resp.Pending = fr.Pending
	}
	common.SuccessResp(c, resp)
}

// FieldsReq backfills extension field groups the initial /fs/list marked pending
// (design §2.6). Refs are the virtual paths (base-path relative, like /fs/list's
// path) whose columns to compute; Fields is the comma-separated group list.
type FieldsReq struct {
	Refs   string `json:"refs" form:"refs"`
	Fields string `json:"fields" form:"fields"`
}

// FsFields computes extension field groups for a set of refs — the backfill for
// groups /fs/list returned as pending (design §2.6). It authorizes each ref for
// the requesting user (read) and hands the decorators ONLY the refs the user may
// see, so it can never be used to probe objects outside the user's view.
// @Summary Backfill extension field groups
// @Description Recomputes the requested extension field groups (e.g. _labels) for a set of refs, for columns /fs/list returned as pending. Each ref is base-path joined and authorized for the user before it is handed to any decorator.
// @Tags fs
// @Security ApiKeyAuth
// @Produce json
// @Param refs query string true "comma-separated virtual paths"
// @Param fields query string true "comma-separated field groups"
// @Success 200 {object} common.Resp[extension.FieldResult]
// @Router /v4/fs/fields [get]
// maxFieldsRefs caps how many refs one /fs/fields backfill may decorate — a
// listing page is at most a screenful, so a larger fan-out is abuse.
const maxFieldsRefs = 500

func FsFields(c *gin.Context) {
	var req FieldsReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	groups := splitFields(req.Fields)
	if len(groups) == 0 {
		common.SuccessResp(c, extension.FieldResult{})
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqRefs := splitFields(req.Refs)
	// Cap the backfill fan-out: a page is at most a screenful, so a caller asking
	// to decorate thousands of refs at once is abuse (each ref is an Authorize +
	// a decorator call). Bound it (F2 red-team: /fs/fields refs were unbounded).
	if len(reqRefs) > maxFieldsRefs {
		reqRefs = reqRefs[:maxFieldsRefs]
	}
	var refs []ext.ObjRef
	for _, p := range reqRefs {
		full, err := user.JoinPath(p)
		if err != nil {
			continue
		}
		if op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: full}) != nil {
			continue // silently drop refs the user may not see
		}
		refs = append(refs, ext.ObjRef{Anchor: full})
	}
	common.SuccessResp(c, extension.DecorateFields(c, extension.DefaultRegistry, groups, refs, 0))
}

// splitFields splits a comma-separated field/ref list, trimming blanks.
func splitFields(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FsDirs lists only the subdirectories of a path.
// @Summary List subdirectories
// @Description Registered for any HTTP method via gin Any(). If force_root is set, requires admin and bypasses the user's base path.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body DirReq true "path/password/force_root"
// @Success 200 {object} common.Resp[[]DirResp]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "password incorrect, no permission, or force_root without admin"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/dirs [post]
// @Router /v4/fs/dirs [get]
func FsDirs(c *gin.Context) {
	var req DirReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	reqPath := req.Path
	if req.ForceRoot {
		if !user.IsAdmin() {
			common.ErrorI18n(c, "errs.permission_denied", 403)
			return
		}
	} else {
		tmp, err := user.JoinPath(req.Path)
		if err != nil {
			common.ErrorResp(c, err, 403)
			return
		}
		reqPath = tmp
	}
	if err := op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: reqPath, Password: req.Password}); err != nil {
		common.AuthzErrorResp(c, err, "password is incorrect or you have no permission")
		return
	}
	objs, err := fs.List(c, reqPath, &fs.ListArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	visible := op.FilterListing(c, user, reqPath, objs)
	dirs := filterDirs(visible)
	common.SuccessResp(c, dirs)
}

type DirResp struct {
	Name     string    `json:"name"`
	Modified time.Time `json:"modified"`
}

func filterDirs(objs []model.Obj) []DirResp {
	var dirs []DirResp
	for _, obj := range objs {
		if obj.IsDir() {
			dirs = append(dirs, DirResp{
				Name:     obj.GetName(),
				Modified: obj.ModTime(),
			})
		}
	}
	return dirs
}

// isEncrypt reports whether reqPath requires a sign query param on raw URLs.
// This is legacy nearest-meta sign semantics (Step 6 will redefine sign
// scoping against EffectiveMeta); kept self-contained here since Task 4
// already removed the caller-side nearest-meta fetch.
func isEncrypt(reqPath string) bool {
	if common.IsStorageSignEnabled(reqPath) {
		return true
	}
	meta, err := op.GetNearestMeta(reqPath)
	if err != nil {
		// MetaNotFound = genuinely no meta → not encrypted. Any other error is
		// a lookup failure: fail closed (require a signature) rather than hand
		// out an unsigned link for a path that might be protected.
		return !errors.Is(err, errs.MetaNotFound) && pkgerr.Cause(err) != errs.MetaNotFound
	}
	if meta.Password == "" {
		return false
	}
	return utils.PathEqual(meta.Path, reqPath) || meta.PSub
}

func normalizeListPage(page, perPage int) (int, int) {
	effPage := page
	if effPage <= 0 {
		effPage = 1
	}
	effPerPage := perPage
	if effPerPage < 0 {
		return effPage, AllPerPage
	}
	if effPerPage == 0 {
		effPerPage = DefaultPerPage
	}
	if effPerPage > MaxPerPage {
		effPerPage = MaxPerPage
	}
	return effPage, effPerPage
}

func calcPagesTotal(total, perPage int) int {
	if perPage == AllPerPage {
		if total > 0 {
			return 1
		}
		return 0
	}
	if total <= 0 || perPage <= 0 {
		return 0
	}
	return (total + perPage - 1) / perPage
}

func pagination(objs []model.Obj, req *model.PageReq) (int, []model.Obj) {
	pageIndex, pageSize := req.Page, req.PerPage
	total := len(objs)
	if pageSize == AllPerPage {
		return total, objs
	}
	start := (pageIndex - 1) * pageSize
	if start > total {
		return total, []model.Obj{}
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return total, objs[start:end]
}

func toObjsResp(objs []model.Obj, parent string, encrypt bool) []ObjListResp {
	var resp []ObjListResp

	for _, obj := range objs {
		thumb, _ := model.GetThumb(obj)
		storageClass, _ := model.GetStorageClass(obj)
		resp = append(resp, ObjListResp{
			Id:           obj.GetID(),
			Path:         obj.GetPath(),
			VirtualPath:  utils.FixAndCleanPath(stdpath.Join(parent, obj.GetName())),
			Name:         obj.GetName(),
			Size:         obj.GetSize(),
			IsDir:        obj.IsDir(),
			Modified:     obj.ModTime(),
			Created:      obj.CreateTime(),
			HashInfoStr:  obj.GetHash().String(),
			HashInfo:     obj.GetHash().Export(),
			Sign:         common.Sign(obj, parent, encrypt),
			Thumb:        thumb,
			Type:         utils.GetObjType(obj.GetName(), obj.IsDir()),
			StorageClass: storageClass,
		})
	}
	return resp
}

type FsGetReq struct {
	Path     string `json:"path" form:"path"`
	Password string `json:"password" form:"password"`
}

type FsGetResp struct {
	ObjResp
	RawURL   string        `json:"raw_url"`
	Readme   string        `json:"readme"`
	Header   string        `json:"header"`
	Provider string        `json:"provider"`
	WebProxy bool          `json:"web_proxy"`
	Related  []ObjListResp `json:"related"`
}

// FsGet returns metadata plus a raw/preview URL for a single file or directory.
// @Summary Get file/directory info
// @Description Registered for any HTTP method via gin Any(). For files, resolves a raw_url (direct link, proxy link, or storage-provided URL depending on driver settings).
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body FsGetReq true "path/password"
// @Success 200 {object} common.Resp[FsGetResp]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "password incorrect or no permission"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/get [post]
// @Router /v4/fs/get [get]
func FsGet(c *gin.Context) {
	var req FsGetReq
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
	if err := op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: reqPath, Password: req.Password}); err != nil {
		common.AuthzErrorResp(c, err, "password is incorrect or you have no permission")
		return
	}
	em, err := op.ResolveEffectiveMeta(reqPath)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	obj, err := fs.Get(c, reqPath, &fs.GetArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	var rawURL string

	storage, storageErr := fs.GetStorage(reqPath, &fs.GetStoragesArgs{})
	provider := "unknown"
	if storageErr == nil {
		provider = storage.Config().Name
	}
	if !obj.IsDir() {
		if storageErr != nil {
			common.ErrorResp(c, storageErr, 500)
			return
		}
		query := ""
		if isEncrypt(reqPath) || setting.GetBool(conf.SignAll) {
			query = "?sign=" + common.SignPath(reqPath)
		}
		forceRedirectRawURL := storage.GetStorage().Driver == "BaiduYouth"
		forcePreviewRawURL := storage.GetStorage().Driver == "Lark" && isLarkCloudDocName(obj.GetName())
		forceProxyRawURL := storage.GetStorage().Driver == "Quark" && utils.GetFileType(obj.GetName()) == conf.VIDEO
		if forceRedirectRawURL {
			// Baidu Youth direct links are minted per request and are not stable enough
			// to expose as fs/get raw_url. Return the local /d endpoint so the frontend
			// obtains a fresh link on each download click.
			rawURL = fmt.Sprintf("%s/d%s%s",
				common.GetApiUrl(c.Request),
				utils.EncodePath(reqPath, true),
				query)
		} else if !forcePreviewRawURL && (storage.Config().MustProxy() || storage.GetStorage().WebProxy || forceProxyRawURL) {
			if storage.GetStorage().DownProxyUrl != "" {
				rawURL = common.BuildDownProxyURL(
					storage.GetStorage().DownProxyUrl,
					reqPath,
					storage.GetStorage().DownProxySign,
				)
			} else {
				rawURL = fmt.Sprintf("%s/p%s%s",
					common.GetApiUrl(c.Request),
					utils.EncodePath(reqPath, true),
					query)
			}
		} else {
			// file have raw url
			if url, ok := model.GetUrl(obj); ok {
				rawURL = url
			} else {
				// if storage is not proxy, use raw url by fs.Link
				link, _, err := fs.Link(c, reqPath, model.LinkArgs{
					IP:       c.ClientIP(),
					Header:   c.Request.Header,
					HttpReq:  c.Request,
					Redirect: true,
				})
				if err != nil {
					common.ErrorResp(c, err, 500)
					return
				}
				common.CompleteLink(c.Request, link)
				rawURL = link.URL
			}
		}
	}
	var related []model.Obj
	parentPath := stdpath.Dir(reqPath)
	sameLevelFiles, err := fs.List(c, parentPath, &fs.ListArgs{})
	if err == nil {
		sameLevelFiles = op.FilterListing(c, user, parentPath, sameLevelFiles)
		related = filterRelated(sameLevelFiles, obj)
	}
	thumb, _ := model.GetThumb(obj)
	storageClass, _ := model.GetStorageClass(obj)
	common.SuccessResp(c, FsGetResp{
		ObjResp: ObjResp{
			Id:           obj.GetID(),
			Path:         obj.GetPath(),
			VirtualPath:  utils.FixAndCleanPath(reqPath),
			Name:         obj.GetName(),
			Size:         obj.GetSize(),
			IsDir:        obj.IsDir(),
			Modified:     obj.ModTime(),
			Created:      obj.CreateTime(),
			HashInfoStr:  obj.GetHash().String(),
			HashInfo:     obj.GetHash().Export(),
			Sign:         common.Sign(obj, parentPath, isEncrypt(reqPath)),
			Type:         utils.GetFileType(obj.GetName()),
			Thumb:        thumb,
			StorageClass: storageClass,
		},
		RawURL:   rawURL,
		Readme:   em.Readme,
		Header:   em.Header,
		Provider: provider,
		WebProxy: storageErr == nil && storage.GetStorage().WebProxy,
		Related:  toObjsResp(related, parentPath, isEncrypt(parentPath)),
	})
}

func filterRelated(objs []model.Obj, obj model.Obj) []model.Obj {
	var related []model.Obj
	nameWithoutExt := strings.TrimSuffix(obj.GetName(), stdpath.Ext(obj.GetName()))
	for _, o := range objs {
		if o.GetName() == obj.GetName() {
			continue
		}
		if strings.HasPrefix(o.GetName(), nameWithoutExt) {
			related = append(related, o)
		}
	}
	return related
}

func isLarkCloudDocName(name string) bool {
	for _, suffix := range []string{
		".lark-doc",
		".lark-docx",
		".lark-sheet",
		".lark-bitable",
		".lark-mindnote",
		".lark-slides",
	} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

type FsOtherReq struct {
	model.FsOtherArgs
	Password string `json:"password" form:"password"`
}

// FsOther invokes a driver-specific "other" method (e.g. decompress, offline-download list) on a path.
// @Summary Driver-specific other operation
// @Description Registered for any HTTP method via gin Any(). The method/data fields are opaque and driver-defined; response shape varies by driver and method.
// @Tags fs
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param body body FsOtherReq true "path/password/method/data"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any] "password incorrect or no permission"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/other [post]
// @Router /v4/fs/other [post]
func FsOther(c *gin.Context) {
	var req FsOtherReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.MustGet(ctxkey.User).(*model.User)
	var err error
	req.Path, err = user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if err := op.Authorize(c, user, model.ActRead, op.AuthzRequest{Path: req.Path, Password: req.Password}); err != nil {
		common.AuthzErrorResp(c, err, "password is incorrect or you have no permission")
		return
	}
	res, err := fs.Other(c, req.FsOtherArgs)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, res)
}
