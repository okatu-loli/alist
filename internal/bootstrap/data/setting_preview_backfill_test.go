package data

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestInitSettingsBackfillsMissingKeyWithoutClobberingCustomizedOnes answers
// the one known gap in porting v3 instances to v4's preview-management
// settings: already-migrated instances won't pick up a later-added settings
// key like preview_archives_by_default unless there's an appended
// no-op-safe migration patch.
//
// Conclusion (verified here, not asserted from memory): this gap is already
// closed by the existing boot sequence, no new migration step is needed.
// initSettings() (setting.go) runs unconditionally on every InitData() call —
// not just fresh installs (see data.go: initUser/initSettings/initTasks all run
// every boot; only markFreshInstall is gated on isNewInstall). For each key in
// the current build's InitialSettings():
//   - if no row exists yet for that key, one is created with the build's
//     current default value (exactly what a genuinely fresh install would get);
//   - if a row already exists, its stored value always wins over the build
//     default, no matter what that default is.
//
// So an instance migrated by a build that predates preview_archives_by_default
// simply has no row for it; the very next boot (any boot — not a special
// upgrade path) creates that row with the then-current default. Nothing about
// this is specific to migratev4 or to this one key: it is the general
// mechanism by which EVERY setting added to a later build reaches an
// already-installed instance.
func TestInitSettingsBackfillsMissingKeyWithoutClobberingCustomizedOnes(t *testing.T) {
	// Establish steady state once, the way InitData's first call in this
	// process would.
	initSettings()

	// An admin has customized an unrelated, pre-existing key — this must
	// survive the next initSettings() run untouched.
	const customTitle = "W4 Backfill Test Title"
	titleItem, err := op.GetSettingItemByKey(conf.SiteTitle)
	if err != nil {
		t.Fatalf("get %s: %v", conf.SiteTitle, err)
	}
	titleItem.Value = customTitle
	if err := op.SaveSettingItem(titleItem); err != nil {
		t.Fatalf("save customized %s: %v", conf.SiteTitle, err)
	}

	// Simulate "this instance predates preview_archives_by_default": its
	// x_setting_items row for the key simply never existed. op's
	// DeleteSettingItemByKey refuses to delete a non-deprecated key (by
	// design, for the admin-facing delete path), so go straight to the db
	// layer, exactly like a database that was never given this row in the
	// first place — the missing-row STATE is what matters, not how it got
	// that way.
	if err := db.DeleteSettingItemByKey(conf.PreviewArchivesByDefault); err != nil {
		t.Fatalf("simulate missing %s: %v", conf.PreviewArchivesByDefault, err)
	}
	if _, err := op.GetSettingItemByKey(conf.PreviewArchivesByDefault); err == nil {
		t.Fatalf("setup broken: %s should be absent after delete", conf.PreviewArchivesByDefault)
	}

	// The next boot: initSettings() runs again, unconditionally.
	initSettings()

	backfilled, err := op.GetSettingItemByKey(conf.PreviewArchivesByDefault)
	if err != nil {
		t.Fatalf("%s was not backfilled by initSettings: %v", conf.PreviewArchivesByDefault, err)
	}
	var wantDefault string
	found := false
	for _, seed := range InitialSettings() {
		if seed.Key == conf.PreviewArchivesByDefault {
			wantDefault = seed.Value
			found = true
			break
		}
	}
	if !found {
		t.Fatal("preview_archives_by_default missing from InitialSettings() — W0 regression")
	}
	if backfilled.Value != wantDefault {
		t.Errorf("backfilled %s = %q, want current build default %q (same as a fresh install)",
			conf.PreviewArchivesByDefault, backfilled.Value, wantDefault)
	}

	// The customization made before the backfilling run must still be intact —
	// the whole point of this test is that filling in ONE missing key must not
	// touch any OTHER key's stored value.
	stillCustom, err := op.GetSettingItemByKey(conf.SiteTitle)
	if err != nil {
		t.Fatalf("get %s after backfill run: %v", conf.SiteTitle, err)
	}
	if stillCustom.Value != customTitle {
		t.Errorf("initSettings clobbered a customized key: %s = %q, want unchanged %q",
			conf.SiteTitle, stillCustom.Value, customTitle)
	}
}
