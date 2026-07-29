package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

// TestSearchIndexSettingsSeeded pins the A1 real-index settings: the four
// backend-selection knobs land in the INDEX display group with the right
// types/flags, and index_progress is SINGLE + PRIVATE internal state.
func TestSearchIndexSettingsSeeded(t *testing.T) {
	// search_index: visible Select over none + the three backend ids.
	si := findSeed(t, conf.SearchIndex)
	if si.Group != model.INDEX {
		t.Errorf("%s: Group = %d, want model.INDEX (%d)", conf.SearchIndex, si.Group, model.INDEX)
	}
	if si.Type != conf.TypeSelect {
		t.Errorf("%s: Type = %q, want %q", conf.SearchIndex, si.Type, conf.TypeSelect)
	}
	if si.Value != "none" {
		t.Errorf("%s: default Value = %q, want %q", conf.SearchIndex, si.Value, "none")
	}
	if si.Options != "none,database_non_full_text,bleve,meilisearch" {
		t.Errorf("%s: Options = %q", conf.SearchIndex, si.Options)
	}
	if si.Flag == model.PRIVATE {
		t.Errorf("%s: must be VISIBLE (not PRIVATE)", conf.SearchIndex)
	}

	// auto_update_index: visible bool default false.
	au := findSeed(t, conf.AutoUpdateIndex)
	if au.Group != model.INDEX {
		t.Errorf("%s: Group = %d, want model.INDEX", conf.AutoUpdateIndex, au.Group)
	}
	if au.Type != conf.TypeBool {
		t.Errorf("%s: Type = %q, want %q", conf.AutoUpdateIndex, au.Type, conf.TypeBool)
	}
	if au.Value != "false" {
		t.Errorf("%s: default Value = %q, want false", conf.AutoUpdateIndex, au.Value)
	}

	// ignore_paths: PRIVATE text default empty.
	ip := findSeed(t, conf.IgnorePaths)
	if ip.Group != model.INDEX {
		t.Errorf("%s: Group = %d, want model.INDEX", conf.IgnorePaths, ip.Group)
	}
	if ip.Type != conf.TypeText {
		t.Errorf("%s: Type = %q, want %q", conf.IgnorePaths, ip.Type, conf.TypeText)
	}
	if ip.Flag != model.PRIVATE {
		t.Errorf("%s: Flag = %d, want model.PRIVATE (%d)", conf.IgnorePaths, ip.Flag, model.PRIVATE)
	}
	if ip.Value != "" {
		t.Errorf("%s: default Value = %q, want empty", conf.IgnorePaths, ip.Value)
	}

	// max_index_depth: PRIVATE number default 20.
	md := findSeed(t, conf.MaxIndexDepth)
	if md.Group != model.INDEX {
		t.Errorf("%s: Group = %d, want model.INDEX", conf.MaxIndexDepth, md.Group)
	}
	if md.Type != conf.TypeNumber {
		t.Errorf("%s: Type = %q, want %q", conf.MaxIndexDepth, md.Type, conf.TypeNumber)
	}
	if md.Flag != model.PRIVATE {
		t.Errorf("%s: Flag = %d, want model.PRIVATE", conf.MaxIndexDepth, md.Flag)
	}
	if md.Value != "20" {
		t.Errorf("%s: default Value = %q, want 20", conf.MaxIndexDepth, md.Value)
	}

	// index_progress: SINGLE + PRIVATE internal state, default "{}".
	pg := findSeed(t, conf.IndexProgress)
	if pg.Group != model.SINGLE {
		t.Errorf("%s: Group = %d, want model.SINGLE (%d)", conf.IndexProgress, pg.Group, model.SINGLE)
	}
	if pg.Type != conf.TypeText {
		t.Errorf("%s: Type = %q, want %q", conf.IndexProgress, pg.Type, conf.TypeText)
	}
	if pg.Flag != model.PRIVATE {
		t.Errorf("%s: Flag = %d, want model.PRIVATE", conf.IndexProgress, pg.Flag)
	}
	if pg.Value != "{}" {
		t.Errorf("%s: default Value = %q, want {}", conf.IndexProgress, pg.Value)
	}
}
