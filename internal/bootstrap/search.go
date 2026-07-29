package bootstrap

import (
	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/search"
	"github.com/alist-org/alist/v3/internal/search/bleve"
	"github.com/alist-org/alist/v3/internal/search/database"
	"github.com/alist-org/alist/v3/internal/search/meilisearch"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// InitSearchBackends registers the concrete SearchIndexer backends as
// independent, manageable built-in extensions (子项目 A, wave A2). Each backend
// is TWO registrations that share an id:
//
//   - RegisterBuiltinExtension inserts a Kind=SearchIndexer registry entry so
//     the backend lists in the extension manager and toggles through
//     EnableBuiltin/DisableBuiltin with persistence — this is the enable/disable
//     boundary the search base's ActiveIndexer gate reads.
//   - search.RegisterBackend records the searcherName→extensionID mapping AND
//     registers the trusted in-process instance with the host registry, so the
//     base can resolve it by the search_index setting AND gate it on the owning
//     extension's enabled state.
//
// A2 shipped builtin.search.database (SQL LIKE, no external deps); A5 added
// builtin.search.bleve (embedded full-text); A6 adds builtin.search.meilisearch
// (external service) ALONGSIDE them — each is a separate manageable extension the
// admin can enable/disable independently, and the search_index setting picks
// which one is live. It must run BEFORE InitBuiltinManage so ApplyDisabledBuiltins
// re-applies an admin's persisted "off" choice to these entries too. A
// registration failure is fatal: a half-wired backend must not run.
func InitSearchBackends() {
	// Backend config now rides on the search base feature's config page as a
	// ConfigSection per backend (design C4): each backend declares a section with
	// Parent="builtin.search" and its own DB setting keys, which the generic
	// FeatureConfigPage(builtin.search) renders alongside the base's general
	// section. The backends no longer contribute a standalone SettingsPages link —
	// the search base feature owns the single page.
	dbManifest := ext.Manifest{
		ID:         database.ExtensionID,
		Name:       map[string]string{"en": "Search: Database index", "zh": "搜索:数据库索引"},
		Kinds:      []ext.Kind{ext.KindSearchIndexer},
		Version:    "builtin",
		Provenance: ext.Provenance{Source: "official"},
		DisableWarning: map[string]string{
			"en": "Disabling this turns off the database search index; file search falls back to a slower, on-the-fly directory scan until it is re-enabled or another backend is selected.",
			"zh": "关闭后数据库搜索索引失效,文件搜索将回落到较慢的实时目录遍历,直到重新启用或选择其他后端。",
		},
		// The database backend has no connection config, so it declares no
		// ConfigSection (and no SettingsPages): nothing to render on the search
		// feature page beyond enable/disable in the manager.
	}
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, dbManifest); err != nil {
		utils.Log.Fatalf("failed to register search backend %q: %+v", dbManifest.ID, err)
	}
	search.RegisterBackend(database.SearcherName, database.ExtensionID, &database.Indexer{})

	bleveManifest := ext.Manifest{
		ID:         bleve.ExtensionID,
		Name:       map[string]string{"en": "Search: Bleve full-text", "zh": "搜索:Bleve 全文"},
		Kinds:      []ext.Kind{ext.KindSearchIndexer},
		Version:    "builtin",
		Provenance: ext.Provenance{Source: "official"},
		DisableWarning: map[string]string{
			"en": "Disabling this turns off the bleve full-text search index; file search falls back to a slower, on-the-fly directory scan until it is re-enabled or another backend is selected.",
			"zh": "关闭后 Bleve 全文搜索索引失效,文件搜索将回落到较慢的实时目录遍历,直到重新启用或选择其他后端。",
		},
		// Bleve's config section on the search feature page (design C4):
		// bleve_index_path, its one DB setting. Parent gates it on the search base.
		Contributions: ext.Contributions{
			ConfigSections: []ext.ConfigSection{{
				Parent:      "builtin.search",
				Title:       map[string]string{"en": "Bleve", "zh": "Bleve"},
				SettingKeys: []string{"bleve_index_path"},
				Order:       2,
			}},
		},
	}
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, bleveManifest); err != nil {
		utils.Log.Fatalf("failed to register search backend %q: %+v", bleveManifest.ID, err)
	}
	search.RegisterBackend(bleve.SearcherName, bleve.ExtensionID, &bleve.Indexer{})

	meiliManifest := ext.Manifest{
		ID:         meilisearch.ExtensionID,
		Name:       map[string]string{"en": "Search: Meilisearch", "zh": "搜索:Meilisearch"},
		Kinds:      []ext.Kind{ext.KindSearchIndexer},
		Version:    "builtin",
		Provenance: ext.Provenance{Source: "official"},
		DisableWarning: map[string]string{
			"en": "Disabling this turns off the Meilisearch search index; file search falls back to a slower, on-the-fly directory scan until it is re-enabled or another backend is selected.",
			"zh": "关闭后 Meilisearch 搜索索引失效,文件搜索将回落到较慢的实时目录遍历,直到重新启用或选择其他后端。",
		},
		// Meilisearch's config section on the search feature page (design C4): its
		// three connection settings. Parent gates it on the search base.
		Contributions: ext.Contributions{
			ConfigSections: []ext.ConfigSection{{
				Parent: "builtin.search",
				Title:  map[string]string{"en": "Meilisearch", "zh": "Meilisearch"},
				SettingKeys: []string{
					"meilisearch_host",
					"meilisearch_api_key",
					"meilisearch_index_prefix",
				},
				Order: 3,
			}},
		},
	}
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, meiliManifest); err != nil {
		utils.Log.Fatalf("failed to register search backend %q: %+v", meiliManifest.ID, err)
	}
	search.RegisterBackend(meilisearch.SearcherName, meilisearch.ExtensionID, &meilisearch.Indexer{})
}
