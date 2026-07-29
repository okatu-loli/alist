package handles

import (
	"encoding/base64"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/hostcap"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// This file is the plugin UI sandbox bridge's controlled backend surface:
// three admin-only endpoints a sandboxed plugin ui/ bundle reaches, through the
// front-end hostBridge, for capabilities it cannot be handed directly inside an
// opaque-origin iframe — a short-lived scoped storage token, a signed object
// link, and its own namespaced KV store. Every endpoint is clamped to the
// plugin's admin-approved Grant (never the manifest) and audited on both the
// allow and deny path, mirroring installOrUpdate/SetPluginGrant's convention.

// pluginUiScopeReq is one requested scoped-token path grant, wire-identical to
// extension.Scope.
type pluginUiScopeReq struct {
	Path  string `json:"path"`
	Write bool   `json:"write,omitempty"`
}

// PluginUiScopedTokenReq is the body of POST /admin/plugins/:id/scoped-token.
type PluginUiScopedTokenReq struct {
	Scopes []pluginUiScopeReq `json:"scopes"`
}

// IssuePluginUiScopedToken mints a short-lived scoped storage token on the
// plugin's behalf, clamped to its manifest-declared storage_tokens. Gate: the
// plugin's approved
// Grant must carry at least one storage_tokens entry — storage_tokens is a
// separate Grant axis from HostAPIs (extension.Grant.StorageTokens), not a
// host_api name.
// @Summary Issue a plugin UI scoped storage token
// @Description Mints a short-lived (<= extension.MaxScopedTokenTTL) scoped storage token for the plugin's sandboxed UI bundle, clamped to the plugin's manifest-declared storage_tokens. 403 if the plugin's approved Grant carries no storage_tokens. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path string true "plugin id"
// @Param body body PluginUiScopedTokenReq true "requested path scopes"
// @Success 200 {object} common.Resp[gin.H]
// @Failure 403 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/scoped-token [post]
func IssuePluginUiScopedToken(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	id := c.Param("id")
	const action = "plugin.ui.scoped-token"

	var req PluginUiScopedTokenReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	grant, ok := mgr.GrantForID(id)
	if !ok {
		common.ErrorStrResp(c, "plugin not installed", 404)
		return
	}
	if grant == nil || len(grant.StorageTokens) == 0 {
		audit.Event(c, user.ID, user.Username, action, "plugin:"+id, false)
		common.ErrorStrResp(c, "plugin was not granted storage_tokens", 403)
		return
	}

	scopes := make([]extension.Scope, len(req.Scopes))
	for i, s := range req.Scopes {
		scopes[i] = extension.Scope{Path: s.Path, Write: s.Write}
	}
	token, err := extension.IssueScopedToken(extension.DefaultRegistry, id, scopes, extension.MaxScopedTokenTTL)
	if err != nil {
		audit.Event(c, user.ID, user.Username, action, "plugin:"+id, false)
		common.ErrorResp(c, err, 400)
		return
	}
	audit.Event(c, user.ID, user.Username, action, "plugin:"+id, true)
	common.SuccessResp(c, gin.H{"token": token})
}

// pluginUiObjRef is the wire shape of ext.ObjRef.
type pluginUiObjRef struct {
	StorageID string `json:"storage_id"`
	Anchor    string `json:"anchor"`
}

// PluginUiSignLinkReq is the body of POST /admin/plugins/:id/sign-link.
type PluginUiSignLinkReq struct {
	Ref   pluginUiObjRef `json:"ref"`
	Scope string         `json:"scope"`
	// TTL is in seconds; <= 0 or omitted lets Signer apply its own default/cap.
	TTL int64 `json:"ttl,omitempty"`
}

// SignPluginUiLink signs a link to an ObjRef on the plugin's behalf, using the
// same grant/config/KV-bound ext.Host pluginproc hands the plugin's own
// subprocess (bootstrap.PluginHost). Gate: the plugin's approved Grant must
// carry the "sign" host_api (hostcap.CapSign).
// @Summary Sign a plugin UI link
// @Description Signs an expiring link to an ObjRef on the plugin's behalf (design §3 Signer), borrowing the same reverse-host pluginproc uses. 403 if the plugin's approved Grant does not include the "sign" host_api. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path string true "plugin id"
// @Param body body PluginUiSignLinkReq true "object ref + scope + ttl"
// @Success 200 {object} common.Resp[gin.H]
// @Failure 403 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/sign-link [post]
func SignPluginUiLink(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	id := c.Param("id")
	const action = "plugin.ui.sign-link"

	var req PluginUiSignLinkReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	h, ok := pluginUiGrantedHost(c, id, hostcap.CapSign, action)
	if !ok {
		return
	}

	ttl := time.Duration(req.TTL) * time.Second
	token, err := h.Sign().SignLink(c.Request.Context(), "", ext.ObjRef{
		StorageID: req.Ref.StorageID,
		Anchor:    req.Ref.Anchor,
	}, ext.SignScope(req.Scope), ttl)
	if err != nil {
		audit.Event(c, user.ID, user.Username, action, "plugin:"+id, false)
		common.ErrorResp(c, err, 400)
		return
	}
	audit.Event(c, user.ID, user.Username, action, "plugin:"+id, true)
	common.SuccessResp(c, gin.H{"token": token})
}

// PluginUiKvValueReq is the body of PUT /admin/plugins/:id/kv/:key.
type PluginUiKvValueReq struct {
	// Value is base64-encoded, since a KV value is opaque bytes.
	Value string `json:"value"`
}

// GetPluginUiKv reads one key from the plugin's own namespaced KV store
// (ext.Store, host-namespaced by plugin id), through the same reverse-host
// pluginproc hands the plugin's own subprocess. Gate: the plugin's approved
// Grant must carry the "store" host_api (hostcap.CapStore). The value is
// returned base64-encoded.
// @Summary Read a plugin UI KV entry
// @Description Reads one key from the plugin's own namespaced KV store on its behalf (design §3 Store/KV), borrowing the same reverse-host pluginproc uses. Value is base64-encoded. 403 if the plugin's approved Grant does not include the "store" host_api. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Produce json
// @Param id path string true "plugin id"
// @Param key path string true "kv key"
// @Success 200 {object} common.Resp[gin.H]
// @Failure 403 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/kv/{key} [get]
func GetPluginUiKv(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	id := c.Param("id")
	key := c.Param("key")
	const action = "plugin.ui.kv.get"

	h, ok := pluginUiGrantedHost(c, id, hostcap.CapStore, action)
	if !ok {
		return
	}

	v, found, err := h.Store().KV().Get(c.Request.Context(), key)
	if err != nil {
		audit.Event(c, user.ID, user.Username, action, "plugin:"+id, false)
		common.ErrorResp(c, err, 400)
		return
	}
	audit.Event(c, user.ID, user.Username, action, "plugin:"+id, true)
	if !found {
		common.SuccessResp(c, gin.H{"value": "", "found": false})
		return
	}
	common.SuccessResp(c, gin.H{"value": base64.StdEncoding.EncodeToString(v), "found": true})
}

// SetPluginUiKv writes one key in the plugin's own namespaced KV store, mirror
// of GetPluginUiKv. Gate: the plugin's approved Grant must carry the "store"
// host_api (hostcap.CapStore).
// @Summary Write a plugin UI KV entry
// @Description Writes one key in the plugin's own namespaced KV store on its behalf (design §3 Store/KV), borrowing the same reverse-host pluginproc uses. Value is base64-encoded in the request body. 403 if the plugin's approved Grant does not include the "store" host_api. Admin only.
// @Tags admin_plugin
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path string true "plugin id"
// @Param key path string true "kv key"
// @Param body body PluginUiKvValueReq true "base64-encoded value"
// @Success 200 {object} common.Resp[any]
// @Failure 400 {object} common.Resp[any]
// @Failure 403 {object} common.Resp[any]
// @Failure 404 {object} common.Resp[any]
// @Router /v4/admin/plugins/{id}/kv/{key} [put]
func SetPluginUiKv(c *gin.Context) {
	user := c.MustGet(ctxkey.User).(*model.User)
	id := c.Param("id")
	key := c.Param("key")
	const action = "plugin.ui.kv.put"

	var req PluginUiKvValueReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	value, err := base64.StdEncoding.DecodeString(req.Value)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	h, ok := pluginUiGrantedHost(c, id, hostcap.CapStore, action)
	if !ok {
		return
	}

	if err := h.Store().KV().Set(c.Request.Context(), key, value); err != nil {
		audit.Event(c, user.ID, user.Username, action, "plugin:"+id, false)
		common.ErrorResp(c, err, 400)
		return
	}
	audit.Event(c, user.ID, user.Username, action, "plugin:"+id, true)
	common.SuccessResp(c, "ok")
}

// pluginUiGrantedHost is the shared gate for the sign-link/kv endpoints: it
// resolves the plugin's reverse-host (bootstrap.PluginHost), 404s if the
// plugin is not installed, then 403s (auditing the denial) unless the plugin's
// approved Grant carries capName. Returns the host and true when the caller may
// proceed; on false it has already written the response.
func pluginUiGrantedHost(c *gin.Context, id, capName, action string) (ext.Host, bool) {
	user := c.MustGet(ctxkey.User).(*model.User)
	mgr, err := extension.DefaultManager()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return nil, false
	}
	grant, installed := mgr.GrantForID(id)
	if !installed {
		common.ErrorStrResp(c, "plugin not installed", 404)
		return nil, false
	}
	if !grant.HasAPI(capName) {
		audit.Event(c, user.ID, user.Username, action, "plugin:"+id, false)
		common.ErrorStrResp(c, "plugin was not granted \""+capName+"\"", 403)
		return nil, false
	}
	h, ok := bootstrap.PluginHost(id)
	if !ok {
		common.ErrorStrResp(c, "plugin not installed", 404)
		return nil, false
	}
	return h, true
}
