package cmd

import (
	"github.com/alist-org/alist/v3/internal/bootstrap/patch/v3_46_0"
	"os"
	"path/filepath"
	"strconv"

	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/bootstrap/data"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/handles"
	log "github.com/sirupsen/logrus"
)

func Init() {
	bootstrap.InitConfig()
	bootstrap.Log()
	bootstrap.InitDB()

	if v3_46_0.IsLegacyRoleDetected() {
		utils.Log.Warnf("Detected legacy role format, executing ConvertLegacyRoles patch early...")
		v3_46_0.ConvertLegacyRoles()
	}

	data.InitData()
	bootstrap.InitPasskey()
	bootstrap.InitTOTP()
	bootstrap.InitLabels()
	bootstrap.InitFrp()
	// Load (or create on first boot) the server's ed25519 reverse-connection
	// identity and wire it so pair/complete can deliver its public key for an
	// agent to pin (design R1). Same DataDir base / injection shape as the
	// plugin host bridge below.
	bootstrap.InitServerIdentity()
	// Start the server-side reverse-connection supervisor (design R4): for every
	// agent configured Mode=="reverse" it DIALS the agent's WS listener, holds a
	// persistent channel, and pushes tasks over it using the handshake-proven
	// id. A true no-op when no reverse agents are configured (spins no
	// goroutine) or when no server identity is wired. Must run after
	// InitServerIdentity so the server private key is available to sign the
	// reverse handshake.
	handles.InitAgentReverseSupervisor()
	// Register the four natively-wired core features (offline download / search /
	// share / SSO) as manageable built-in extensions. Placed after the other
	// built-in registrations and before InitBuiltinManage so ApplyDisabledBuiltins
	// re-applies an admin's persisted disable choice to them too.
	bootstrap.InitCoreFeatureExtensions()
	// Register the concrete search-index backends (子项目 A: builtin.search.database)
	// as independent manageable built-ins + trusted in-process instances. Placed
	// with InitCoreFeatureExtensions and before InitBuiltinManage so
	// ApplyDisabledBuiltins re-applies an admin's persisted disable choice to them.
	bootstrap.InitSearchBackends()
	// Register the concrete OfflineDownloadTool backends (子项目 B:
	// builtin.offline_download.aria2) as independent manageable built-ins +
	// trusted in-process instances, exactly like InitSearchBackends. Placed right
	// after it and before InitBuiltinManage so ApplyDisabledBuiltins re-applies an
	// admin's persisted disable choice to them too.
	bootstrap.InitOfflineDownloadTools()
	// Register the built-in zip ArchiveTool (Phase E wave 2:
	// builtin.archive.zip) as an independent manageable built-in + trusted
	// in-process instance, exactly like InitSearchBackends/
	// InitOfflineDownloadTools. Placed right after it and before
	// InitBuiltinManage so ApplyDisabledBuiltins re-applies an admin's
	// persisted disable choice to it too.
	bootstrap.InitArchiveTools()
	// Register the built-in still-image Thumbnailer (Phase E wave 3:
	// builtin.thumbnail.image) as an independent manageable built-in +
	// trusted in-process instance, exactly like InitArchiveTools. Placed
	// right after it and before InitBuiltinManage so ApplyDisabledBuiltins
	// re-applies an admin's persisted disable choice to it too.
	bootstrap.InitThumbnailers()
	// After every Init* that registers a built-in extension and before
	// StartBackgroundServices (cmd/server.go): re-apply the persisted disabled
	// set so a built-in an admin turned off stays off across restarts.
	bootstrap.InitBuiltinManage()
	// Wire the reverse-host builder so a launched StorageDriver subprocess gets a
	// grant-bound ext.Host (its Init is called; it can borrow FS/Store/HTTP/config).
	bootstrap.InitPluginHostBridge()
	bootstrap.InitObjRelocateHandlers()
	bootstrap.InitStreamLimit()
	bootstrap.InitUpgradePatch()
}

func Release() {
	db.Close()
}

var pid = -1
var pidFile string

func initDaemon() {
	ex, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	exPath := filepath.Dir(ex)
	_ = os.MkdirAll(filepath.Join(exPath, "daemon"), 0700)
	pidFile = filepath.Join(exPath, "daemon/pid")
	if utils.Exists(pidFile) {
		bytes, err := os.ReadFile(pidFile)
		if err != nil {
			log.Fatal("failed to read pid file", err)
		}
		id, err := strconv.Atoi(string(bytes))
		if err != nil {
			log.Fatal("failed to parse pid data", err)
		}
		pid = id
	}
}
