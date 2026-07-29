package bootstrap

import (
	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/archive"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// InitArchiveTools registers the built-in zip ArchiveTool as an independent,
// manageable built-in extension (Phase E wave 2), mirroring InitSearchBackends
// / InitOfflineDownloadTools: RegisterBuiltinExtension inserts a
// Kind=ArchiveTool registry entry so it lists in the extension manager and
// toggles through EnableBuiltin/DisableBuiltin with persistence, and
// extension.RegisterTrustedArchiveToolInstance registers the trusted
// in-process instance the internal/archive dispatch layer resolves by file
// extension (FindArchiveToolForExt) -- disabling this extension makes a .zip
// path fall back to the exact same ErrNotArchive an unsupported format
// already returns, since no tool then claims "zip".
//
// It must run BEFORE InitBuiltinManage so ApplyDisabledBuiltins re-applies an
// admin's persisted "off" choice to this entry too. A registration failure is
// fatal: a half-wired archive tool must not run.
func InitArchiveTools() {
	zipManifest := ext.Manifest{
		ID:         archive.ZipToolExtensionID,
		Name:       map[string]string{"en": "Archive: ZIP", "zh": "归档:ZIP"},
		Kinds:      []ext.Kind{ext.KindArchiveTool},
		Version:    "builtin",
		Provenance: ext.Provenance{Source: "official"},
		DisableWarning: map[string]string{
			"en": "Disabling this removes zip archive browsing (fs/archive/meta, list, extract); a .zip path behaves like any other unsupported archive format until it is re-enabled.",
			"zh": "关闭后 zip 归档浏览(fs/archive/meta、list、extract)将不可用,.zip 文件在重新启用前会表现为不受支持的归档格式。",
		},
	}
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, zipManifest); err != nil {
		utils.Log.Fatalf("failed to register archive tool %q: %+v", zipManifest.ID, err)
	}
	extension.RegisterTrustedArchiveToolInstance(archive.ZipToolExtensionID, archive.NewZipArchiveTool())
}
