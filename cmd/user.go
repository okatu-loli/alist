package cmd

import (
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// DelAdminCacheOnline clears the in-memory cache entry for the current admin
// user. Called after CLI password rotation (admin set / admin random) so a
// running server picks up the change immediately instead of serving a stale
// cached admin user.
func DelAdminCacheOnline() {
	admin, err := op.GetAdmin()
	if err != nil {
		utils.Log.Errorf("[del_admin_cache] get admin error: %+v", err)
		return
	}
	DelUserCacheOnline(admin.Username)
}

// DelUserCacheOnline evicts the in-memory cache entry for username.
//
// This used to make an HTTP self-call to POST /api/admin/user/del_cache,
// authenticated with conf.Token as the bearer credential. Since the admin
// API token is hashed at rest by default (pkg/utils.HashToken; see
// conf.TokenPlaintext), setting.GetStr(conf.Token) returns the salted digest,
// not the raw token, so that self-call would fail middlewares.Auth's
// utils.VerifyToken check and 401 — silently leaving the online cache stale.
//
// The CLI runs in the same process as the server (cmd.Init wires up the same
// op/db layer), so there is no need to round-trip through HTTP and API-token
// auth just to invalidate an in-memory map: call the cache-clearing function
// directly.
func DelUserCacheOnline(username string) {
	if err := op.DelUserCache(username); err != nil {
		utils.Log.Warnf("[del_user_cache_online] failed: %+v", err)
		return
	}
	utils.Log.Debugf("[del_user_cache_online] del user [%s] cache success", username)
}
