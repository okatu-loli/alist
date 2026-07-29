package handles

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/pluginproc"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// RegisterDevPluginRoutes mounts the dev-only hot-load endpoint on the plugins
// route group, but ONLY when the server is running with --dev (flags.Dev) —
// otherwise it is a no-op and the route is never present. DevLoadPlugin
// executes an arbitrary local binary path with no signature check, so
// RequireAdmin alone (already applied to the whole group) is not a sufficient
// gate for a production deployment; the route must not exist at all unless the
// operator opted into dev mode. As a second line of defense (DevLoadPlugin
// itself also rejects non-loopback callers), mounting it always logs a
// startup warning: a --dev instance is expected to run on a developer's own
// machine, but if one is ever reachable beyond localhost, dev/load — combined
// with, say, the documented initial admin/12345678 window — is an
// unauthenticated-feeling path to remote code execution.
func RegisterDevPluginRoutes(g *gin.RouterGroup) {
	if !flags.Dev {
		return
	}
	log.Warnf("plugin dev: --dev mounted POST .../admin/plugins/dev/load — it executes unsigned local plugin binaries with no plugin_policy/install-wizard gate, restricted to loopback callers only; never expose a --dev server beyond localhost")
	g.POST("/dev/load", DevLoadPlugin)
}

// remoteAddrIsLoopback reports whether remoteAddr (an http.Request.RemoteAddr
// - "host:port", the net.Conn peer address the Go HTTP server itself
// observed) resolves to a loopback IP. Used instead of gin's c.ClientIP() by
// DevLoadPlugin's loopback gate - see the comment there for why ClientIP()
// would be spoofable here. If remoteAddr has no ":port" suffix (unexpected
// in a real request, but harmless to handle) the whole string is tried as a
// bare IP; anything that fails to parse as an IP is treated as non-loopback
// (fail closed).
func remoteAddrIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DevLoadPlugin loads (or hot-reloads) an UNSIGNED plugin straight from a
// local directory. DEV ONLY: it executes an arbitrary binary path, so the
// route is mounted only when the server runs with --dev (see
// RegisterDevPluginRoutes) — RequireAdmin alone is NOT the gate.
//
// It re-registers the plugin's manifest/binary in the registry (free of the
// trust-tier gate normal installs go through — a dev loop rebuilds a local,
// unsigned binary on every iteration), evicts any already-running subprocess
// so the next dispatch launches the freshly built binary, and replays
// op.LoadStorage for every mounted storage backed by this plugin id so an
// admin doesn't have to manually re-add storages to see the new code.
func DevLoadPlugin(c *gin.Context) {
	// dev/load's whole threat model assumes the plugin dev CLI and this
	// server are the same machine — the CLI pushes filesystem paths that are
	// only meaningful locally, and there is no signature check at all.
	// RequireAdmin alone is not a sufficient gate for a --dev instance that
	// ends up reachable beyond localhost, so reject any non-loopback caller
	// here, before doing anything else (including reading the body).
	//
	// Deliberately checks c.Request.RemoteAddr, NOT c.ClientIP(): gin's
	// ClientIP() defaults to trusting X-Forwarded-For (and the other
	// RemoteIPHeaders) from ANY peer unless Engine.SetTrustedProxies has
	// narrowed that set — which this codebase never calls. That means
	// ClientIP() would accept a trivially spoofable
	// "X-Forwarded-For: 127.0.0.1" header sent by an arbitrary public-
	// internet caller, making a ClientIP()-based gate a no-op against
	// exactly the attacker this check exists to stop. RemoteAddr is the
	// net.Conn peer address the Go HTTP server itself observed — it cannot
	// be forged by any request header.
	if !remoteAddrIsLoopback(c.Request.RemoteAddr) {
		err := fmt.Errorf("plugins/dev/load only accepts requests from the loopback interface (remote addr: %q)", c.Request.RemoteAddr)
		common.ErrorResp(c, err, 403)
		return
	}

	var req struct {
		Dir string `json:"dir" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	user := c.MustGet(ctxkey.User).(*model.User)

	man, err := extension.LoadManifest(req.Dir)
	if err != nil {
		audit.Event(c, user.ID, user.Username, "plugin.dev.load", "dir:"+req.Dir, false)
		common.ErrorResp(c, err, 400)
		return
	}

	// Reject the reserved builtin.* namespace BEFORE touching the registry at
	// all (and before even resolving a binary — this is a pure namespace
	// check on the manifest). RegisterPluginMode already rejects this prefix,
	// but only AFTER the Deregister below would already have run — Deregister
	// has no such guard, so a dev plugin.json that (accidentally or not)
	// claims a builtin id would rip out a live built-in feature (labels/SSO/
	// passkey/FRP…) first and then fail to re-register it, leaving the server
	// with that built-in permanently gone until restart. Checking here keeps
	// Deregister from ever running against a builtin id in the first place.
	if strings.HasPrefix(man.ID, "builtin.") {
		err := fmt.Errorf("extension %q: the 'builtin.' namespace is reserved for built-in features", man.ID)
		audit.Event(c, user.ID, user.Username, "plugin.dev.load", "plugin:"+man.ID, false)
		common.ErrorResp(c, err, 400)
		return
	}

	bin, err := extension.FindBinary(req.Dir)
	if err != nil {
		audit.Event(c, user.ID, user.Username, "plugin.dev.load", "plugin:"+man.ID, false)
		common.ErrorResp(c, err, 400)
		return
	}

	// Free of the trust-tier/policy gate a normal install/enable goes through
	// (extension.Manager.gate): a dev loop reloads a locally rebuilt, unsigned
	// binary on every iteration, and it's already fenced by flags.Dev + admin
	// auth above. Deregister first so a repeated dev/load (rebuild-and-reload)
	// is idempotent instead of erroring on an existing id.
	extension.DefaultRegistry.Deregister(man.ID)
	if err := extension.DefaultRegistry.RegisterPluginMode(man, bin, extension.ExecGRPC); err != nil {
		audit.Event(c, user.ID, user.Username, "plugin.dev.load", "plugin:"+man.ID, false)
		common.ErrorResp(c, err, 500)
		return
	}

	// Evict and the storage reload below deliberately do NOT use the request
	// context: an admin/dev CLI closing the connection (or a client-side
	// timeout) mid-request must not abort a subprocess relaunch or a storage
	// reload half-way through — that would leave the old subprocess already
	// killed, the new one's handshake canceled, and pluginproc's cached
	// client pointing at a dead process (same failure shape for a partially
	// replayed storage loop: some storages reloaded, the rest silently
	// skipped with no way to tell from the response). context.WithoutCancel
	// keeps request-scoped values (e.g. trace/log fields) but drops the
	// cancellation signal, so these two steps always run to completion once
	// started.
	detached := context.WithoutCancel(c.Request.Context())

	// Evict any cached subprocess for this id so the next dispatch launches
	// the freshly built binary instead of reusing the old one. No-op (nil) if
	// nothing was running yet.
	if err := pluginproc.Default.Evict(detached, man.ID, bin); err != nil {
		log.Errorf("plugin dev load %s: evict subprocess: %v", man.ID, err)
	}

	// Replay LoadStorage for every already-mounted storage backed by this
	// plugin id so the hot-reload is actually visible without the admin
	// manually re-adding storages. A single storage's reload failing (e.g. a
	// binary that no longer accepts its old Addition) is logged, not fatal to
	// the request — the manifest/registry swap above already succeeded.
	reloaded := 0
	for _, d := range op.GetAllStorages() {
		st := d.GetStorage()
		if st == nil || st.Driver != man.ID {
			continue
		}
		if err := op.LoadStorage(detached, *st); err != nil {
			log.Errorf("plugin dev load %s: reload storage %d (%s): %v", man.ID, st.ID, st.MountPath, err)
			continue
		}
		reloaded++
	}

	audit.Event(c, user.ID, user.Username, "plugin.dev.load", "plugin:"+man.ID, true)
	common.SuccessResp(c, gin.H{
		"id":                man.ID,
		"version":           man.Version,
		"kinds":             man.Kinds,
		"reloaded_storages": reloaded,
	})
}
