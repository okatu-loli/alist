package offline_download

import (
	"sync"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// AvailableToolNames lists the offline-download tools a caller may pick for a
// per-request add: always "SimpleHttp" first (the always-available base direct
// path), followed by every registered tool name that currently resolves through
// the enabled-gate (ToolByName != nil), in the sorted order the host registry
// enumerates them. A registered-but-disabled tool (its owning extension off) is
// omitted, so the list matches exactly what AddOfflineDownload will accept.
func AvailableToolNames() []string {
	names := extension.OfflineDownloadToolInstanceIDs() // already sorted
	out := make([]string, 0, len(names)+1)
	out = append(out, "SimpleHttp")
	for _, name := range names {
		if ToolByName(name) != nil {
			out = append(out, name)
		}
	}
	return out
}

// This file adds the daemon-tool base ALONGSIDE the built-in SimpleHttp direct
// path in task.go / simplehttp.go (子项目 B, wave B1). It does NOT replace or
// touch that path: an empty/SimpleHttp/none selection keeps DownloadTask on the
// existing direct op.Put flow (the always-available base, like the BFS in
// search). The base holds the currently active ext.OfflineDownloadTool (resolved
// from the offline_download_tool setting via the host registry) and exposes a
// thin resolver a later wave routes daemon DownloadTasks through.
//
// Naming: this package has no existing Init; the active-tool resolver is named
// Init to mirror search.Init exactly (a hook fires it on setting change). It is
// distinct from the package's DownloadTask.Run direct-download logic, which B1
// leaves untouched.
var (
	activeMu   sync.RWMutex
	activeTool ext.OfflineDownloadTool

	// toolExtID maps a tool's Name (Config().Name, what the
	// offline_download_tool setting selects on, e.g. "aria2") to the manageable
	// extension id that owns it (e.g. "builtin.offline_download.aria2"). The two
	// deliberately differ, so this indirection lets ActiveTool gate on whether
	// the OWNING extension is enabled — disabling that extension takes the tool
	// offline (offline download falls back to SimpleHttp) with no manager hook.
	// Populated only by RegisterTool and read under activeMu.
	toolExtID = map[string]string{}
)

// RegisterTool records a tool's toolName→extensionID mapping AND registers its
// trusted in-process instance with the host registry in one step. A tool
// extension's bootstrap uses this instead of calling
// extension.RegisterTrustedOfflineDownloadToolInstance directly, so the gating
// map is never left unpopulated (an unmapped active tool would be treated as
// always disabled by ActiveTool). A nil instance clears both the mapping and the
// host registration.
func RegisterTool(toolName, extensionID string, t ext.OfflineDownloadTool) {
	activeMu.Lock()
	if t == nil {
		delete(toolExtID, toolName)
	} else {
		toolExtID[toolName] = extensionID
	}
	activeMu.Unlock()
	extension.RegisterTrustedOfflineDownloadToolInstance(toolName, t)
}

func init() {
	// Changing offline_download_tool re-resolves the active daemon tool. Mirrors
	// search_index driving which searcher is live.
	op.RegisterSettingItemHook(conf.OfflineDownloadTool, func(item *model.SettingItem) error {
		return Init(item.Value)
	})
}

// Init resolves the active OfflineDownloadTool from mode (the
// offline_download_tool value). mode=="" || "SimpleHttp" || "none" releases any
// active tool and leaves the base on the built-in SimpleHttp direct path.
// Otherwise it looks the id up in the host registry: a hit becomes the active
// instance; a miss is NOT an error (a not-yet-registered or disabled tool simply
// means offline download falls back to SimpleHttp), so the instance is cleared
// and nil is returned. Switching to a different tool releases the previous one
// first.
func Init(mode string) error {
	activeMu.Lock()
	defer activeMu.Unlock()

	if mode == "" || mode == "SimpleHttp" || mode == "none" {
		release()
		return nil
	}

	t, ok := extension.GetOfflineDownloadTool(mode)
	if !ok {
		// Unregistered/disabled tool: not an error, just no active tool.
		release()
		return nil
	}

	// Already active under the same identity: nothing to do.
	if activeTool != nil && activeTool.Config().Name == t.Config().Name {
		return nil
	}
	// Switching tools: drop the previous one before adopting the new.
	release()
	activeTool = t
	return nil
}

// release drops the reference to the active tool. The OfflineDownloadTool
// instances are owned by the host extension registry
// (RegisterTrustedOfflineDownloadToolInstance), which drives their real
// lifecycle (Init/Shutdown); the base only holds the "active" pointer, so
// releasing is just clearing that pointer. Callers hold activeMu.
func release() {
	activeTool = nil
}

// ToolByName resolves a registered OfflineDownloadTool by its tool NAME (the
// offline_download_tool value / DownloadTask.Toolname, e.g. "aria2"), applying
// the SAME enabled-gate as ActiveTool: the name must resolve in the host
// registry AND its owning extension (toolExtID[name]) must be present and
// Enabled(). Returns nil for an empty/SimpleHttp/none selection, an
// unregistered/unmapped name, or a disabled owning extension — the same "use the
// built-in SimpleHttp direct path" signal ActiveTool returns.
//
// This is what a per-request/per-task tool selection resolves through: a
// DownloadTask created for "aria2" calls ToolByName("aria2") at run time, so it
// keeps using aria2 even if the global offline_download_tool setting later
// switches to a different tool. ActiveTool is the setting-driven special case
// (ToolByName of the currently-selected tool).
//
// Miss fallback (Phase E ring 8): offlineDownloadToolInstances (what
// GetOfflineDownloadTool checks) only ever holds the in-process built-in tools
// registered by RegisterTool above — a Kind=OfflineDownloadTool gRPC plugin is
// never registered there. So a miss here does not immediately mean "no tool":
// it falls through to extension.FindOfflineDownloadToolByName, which scans the
// host registry's enabled Kind=OfflineDownloadTool gRPC entries for one whose
// self-reported Config().Name matches name. That scan already applies its own
// enabled-gate (Registry.ByKind only returns enabled entries), so a disabled
// gRPC tool naturally falls through to nil here too, matching the built-in
// path's behavior.
func ToolByName(name string) ext.OfflineDownloadTool {
	if name == "" || name == "SimpleHttp" || name == "none" {
		return nil
	}
	t, ok := extension.GetOfflineDownloadTool(name)
	if ok {
		activeMu.RLock()
		extID := toolExtID[name]
		activeMu.RUnlock()
		if e, ok := extension.DefaultRegistry.Get(extID); !ok || !e.Enabled() {
			// Unmapped or disabled owning extension: gate closed.
			return nil
		}
		return t
	}
	// Not a registered built-in name: try the gRPC miss-fallback before
	// giving up.
	if gt, ok := extension.FindOfflineDownloadToolByName(extension.DefaultRegistry, name); ok {
		return gt
	}
	return nil
}

// ActiveTool returns the currently active OfflineDownloadTool, or nil when none
// is active (offline_download_tool == SimpleHttp/none, the selected tool is not
// registered, or — the enabled-gate — the tool's OWNING extension is currently
// disabled). A nil return is the signal to use the built-in SimpleHttp direct
// path.
//
// It is the setting-driven case of ToolByName: whatever tool the
// offline_download_tool setting last resolved to (activeTool) re-checked through
// ToolByName's registry lookup + enabled-gate at RESOLUTION time, so an admin
// disabling builtin.offline_download.aria2 makes offline download fall back to
// SimpleHttp on the very next add, without any manager→offline_download callback.
func ActiveTool() ext.OfflineDownloadTool {
	activeMu.RLock()
	t := activeTool
	activeMu.RUnlock()
	if t == nil {
		return nil
	}
	return ToolByName(t.Config().Name)
}

// ActiveToolName reports the active tool's name, or "" when none is active
// (honoring the enabled-gate via ActiveTool). "" means "use the built-in
// SimpleHttp direct path".
func ActiveToolName() string {
	t := ActiveTool()
	if t == nil {
		return ""
	}
	return t.Config().Name
}
