package bootstrap

import (
	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/thumbnail"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// InitThumbnailers registers the built-in still-image Thumbnailer as an
// independent, manageable built-in extension (Phase E wave 3), mirroring
// InitArchiveTools: RegisterBuiltinExtension inserts a Kind=Thumbnailer
// registry entry so it lists in the extension manager and toggles through
// EnableBuiltin/DisableBuiltin with persistence, and
// extension.RegisterTrustedThumbnailerInstance registers the trusted
// in-process instance internal/thumbnail's Generate resolves by the source
// file's extension (FindThumbnailerForExt) -- disabling this extension makes
// every image path fall back to the exact same ErrNotImageThumbnailable an
// unsupported extension already returns, since no thumbnailer then claims
// it.
//
// It must run BEFORE InitBuiltinManage so ApplyDisabledBuiltins re-applies an
// admin's persisted "off" choice to this entry too. A registration failure is
// fatal: a half-wired thumbnailer must not run.
func InitThumbnailers() {
	imageManifest := ext.Manifest{
		ID:         thumbnail.ImageThumbnailerExtensionID,
		Name:       map[string]string{"en": "Thumbnail: Image", "zh": "缩略图:图片"},
		Kinds:      []ext.Kind{ext.KindThumbnailer},
		Version:    "builtin",
		Provenance: ext.Provenance{Source: "official"},
		DisableWarning: map[string]string{
			"en": "Disabling this removes image thumbnail generation (fs/thumb); an image path behaves like any other non-thumbnailable file until it is re-enabled.",
			"zh": "关闭后图片缩略图生成(fs/thumb)将不可用,图片文件在重新启用前会表现为不支持缩略图的文件。",
		},
	}
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, imageManifest); err != nil {
		utils.Log.Fatalf("failed to register thumbnailer %q: %+v", imageManifest.ID, err)
	}
	extension.RegisterTrustedThumbnailerInstance(thumbnail.ImageThumbnailerExtensionID, thumbnail.NewImageThumbnailer())
}
