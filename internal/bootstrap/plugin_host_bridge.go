package bootstrap

import (
	"encoding/json"
	"path/filepath"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/hostcap"
	"github.com/alist-org/alist/v3/internal/extension/pluginproc"
	log "github.com/sirupsen/logrus"
)

// This file, not internal/extension, is where a persisted extension.Grant is
// mapped to hostcap.Grant and turned into a reverse ext.Host factory. That
// mapping necessarily imports internal/extension/hostcap, and hostcap already
// imports internal/fs and internal/op — both of which import internal/extension
// (see internal/extension/grant.go's GrantItem comment) — so internal/extension
// itself can never import hostcap without a cycle. internal/bootstrap sits above
// all three (nothing in extension/hostcap/fs/op imports it), so the wiring lives
// here instead.

// toHostcapGrant maps a persisted extension.Grant to the hostcap.Grant the
// borrowed-capability layer enforces. host_api names share the hostcap
// vocabulary (host.fs/host.auth/store/sign/network/cache), so they copy 1:1;
// Network copies as the effective allowlist; FS scopes are empty (a StorageDriver
// borrows no AList-fs subtree by default). StorageTokens/Filesystem-level are
// enforced on other axes (scoped tokens / local disk), not via hostcap.Grant.
func toHostcapGrant(pluginID string, g *extension.Grant) hostcap.Grant {
	hg := hostcap.Grant{PluginID: pluginID}
	if g == nil {
		return hg
	}
	for _, it := range g.HostAPIs {
		hg.HostAPIs = append(hg.HostAPIs, hostcap.GrantItem{Name: it.Name, Scope: it.Scope})
	}
	hg.Network = append([]string(nil), g.Network...)
	return hg
}

// secretsSubdir is the DiskKVBackend root for Store().Secrets(), a sibling of
// the plaintext KV's <Manager.Dir()>/<pluginID>/kv.json tree — disjoint
// storage so a secret can never be read back through the plaintext KV leg (or
// vice versa) regardless of what key string a plugin uses. Dot-prefixed so it
// can never collide with a real plugin id directory.
const secretsSubdir = ".secrets"

// hostBuilderFor returns the per-plugin reverse-host factory pluginproc calls on
// every launch. Each launched subprocess gets an ext.Host bound to that plugin's
// grant (capability gating) and a ConfigLoader reading its persisted config.
func hostBuilderFor(m *extension.Manager) func(pluginID string) ext.Host {
	// One persistent, disk-backed KV instance shared by every host this builder
	// produces — a plugin relaunch (and even a process restart, since a fresh
	// Manager + hostBuilderFor over the same dir re-reads the same files) sees
	// its prior KV data instead of starting empty (see diskkv.go).
	sharedKV := hostcap.NewDiskKVBackend(m.Dir())

	// Secrets: a disk-backed KV disjoint from sharedKV's storage extent
	// (secretsSubdir), wrapped so every value is AES-256-GCM sealed before it
	// touches disk. The key is generated once and persisted at
	// <Manager.Dir()>/.secrets.key (0600, atomic write) so it, and therefore
	// every secret, survives a process restart. If the key cannot be
	// loaded/created (e.g. an unwritable data dir), fail closed: log and leave
	// the reverse-host bridge off entirely, mirroring InitPluginHostBridge's
	// own no-manager fallback below, rather than silently handing out an
	// ephemeral key that would make every previously stored secret
	// permanently undecryptable on next restart.
	secretsKey, err := hostcap.LoadOrCreateSecretsKey(m.Dir())
	if err != nil {
		log.Errorf("plugin host bridge: load/create secrets key: %+v (Store().Secrets() will use a non-persistent key for this run)", err)
	}
	var sharedSecrets hostcap.KVBackend
	if err == nil {
		sharedSecrets, err = hostcap.NewEncryptingKVBackend(hostcap.NewDiskKVBackend(filepath.Join(m.Dir(), secretsSubdir)), secretsKey)
		if err != nil {
			log.Errorf("plugin host bridge: build secrets backend: %+v (Store().Secrets() will use a non-persistent key for this run)", err)
		}
	}
	// sharedSecrets stays nil on either failure above; hostcap.New's default
	// (an ephemeral, never-persisted-key MemKVBackend) takes over per plugin —
	// Secrets() still works and is still encrypted, it just does not survive a
	// restart, which is the closest available fail-safe short of disabling the
	// capability outright.

	return func(pluginID string) ext.Host {
		g, _ := m.GrantForID(pluginID)
		return hostcap.New(toHostcapGrant(pluginID, g), hostcap.Options{
			KVBackend:      sharedKV,
			SecretsBackend: sharedSecrets,
			// grant.Network is the real allowlist; global egress here is
			// permissive on purpose (a deny-all default would block every
			// granted host too).
			OutboundPolicy: hostcap.AllowAllOutbound,
			ConfigLoader:   func() ([]byte, error) { return m.GetConfig(pluginID) },
			ConfigSaver:    func(cfg []byte) error { return m.SetConfig(pluginID, json.RawMessage(cfg)) },
		})
	}
}

// sharedBuilder is the process-wide reverse-host builder over
// extension.DefaultManager(), built exactly once (sharedBuilderOnce) so its
// closed-over KV singleton (hostBuilderFor's sharedKV) is shared by every
// caller — pluginproc's launched subprocesses AND the admin UI-bridge
// endpoints (server/handles/plugin_ui.go) via PluginHost below. Building it
// twice would hand out two independent KV instances that, while both
// disk-backed at the same path, defeat the in-process caching/consistency
// hostBuilderFor's doc comment describes as "shared by every host this
// builder produces".
var (
	sharedBuilderOnce sync.Once
	sharedBuilder     func(pluginID string) ext.Host
)

// builderFor returns the process-wide singleton reverse-host builder over m,
// constructing it on the first call.
func builderFor(m *extension.Manager) func(pluginID string) ext.Host {
	sharedBuilderOnce.Do(func() {
		sharedBuilder = hostBuilderFor(m)
	})
	return sharedBuilder
}

// InitPluginHostBridge wires the reverse-host builder into the process-wide
// pluginproc manager so a launched StorageDriver subprocess gets a working,
// grant-bound ext.Host (its Init is called; it can borrow FS/Store/HTTP/config).
func InitPluginHostBridge() {
	mgr, err := extension.DefaultManager()
	if err != nil {
		return // no manager (e.g. minimal boot) → leave reverse host off
	}
	pluginproc.Default.SetHostBuilder(builderFor(mgr))
}

// PluginHost builds the reverse-host for a plugin id — the SAME grant/config/KV
// -bound ext.Host pluginproc hands the plugin's own subprocess — so admin
// UI-bridge endpoints can borrow Sign/Store on the plugin's behalf, clamped to
// its grant (server/handles/plugin_ui.go). Returns false if the plugin is not
// installed (or no manager is available). It goes through builderFor so the KV
// singleton stays the one shared instance rather than a fresh one per call.
func PluginHost(id string) (ext.Host, bool) {
	mgr, err := extension.DefaultManager()
	if err != nil {
		return nil, false
	}
	if _, ok := mgr.GrantForID(id); !ok {
		return nil, false
	}
	return builderFor(mgr)(id), true
}
