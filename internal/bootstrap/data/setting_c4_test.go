package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestSearchBackendSettingsSeeded pins the C4 migration of the search backends'
// connection config from the config file to DB settings: bleve_index_path,
// meilisearch_host and meilisearch_index_prefix land VISIBLE (an admin edits them
// on the search feature page) while meilisearch_api_key lands PRIVATE (it is a
// credential), all as TypeString entries in the INDEX display group with the
// expected defaults.
func TestSearchBackendSettingsSeeded(t *testing.T) {
	cases := []struct {
		key     string
		wantDef string
		private bool
	}{
		{conf.BleveIndexPath, "", false},
		{conf.MeilisearchHost, "http://localhost:7700", false},
		{conf.MeilisearchAPIKey, "", true},
		{conf.MeilisearchIndexPrefix, "", false},
	}
	for _, tc := range cases {
		it := findSeed(t, tc.key)
		if it.Group != model.INDEX {
			t.Errorf("%s: Group = %d, want INDEX (%d)", tc.key, it.Group, model.INDEX)
		}
		if it.Type != conf.TypeString {
			t.Errorf("%s: Type = %q, want %q", tc.key, it.Type, conf.TypeString)
		}
		if it.Value != tc.wantDef {
			t.Errorf("%s: default Value = %q, want %q", tc.key, it.Value, tc.wantDef)
		}
		if tc.private && it.Flag != model.PRIVATE {
			t.Errorf("%s: must be PRIVATE (it is a credential)", tc.key)
		}
		if !tc.private && it.Flag == model.PRIVATE {
			t.Errorf("%s: must be VISIBLE (not PRIVATE) so an admin can configure it", tc.key)
		}
	}
}
