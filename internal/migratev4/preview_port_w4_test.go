package migratev4

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// previewSettingsFixture is a real-shaped snapshot of the v3 "预览管理" setting
// rows. Every value is deliberately customized away from the v4 build default
// (so a test bug that silently fell back to the default couldn't pass by
// coincidence), and the two JSON blobs (iframe_previews/preview_settings) carry
// a comma-separated extension group ("ppt,pptx" inside the doc,docx,... group),
// a /regex/flags group (grouping keys support not just comma-separated lists
// but also a "/regex/flags" form), nested objects and irregular
// whitespace/key order — so a round trip that silently
// re-parsed/re-serialized the JSON (instead of carrying the raw string as-is,
// unparsed and unmodified) would be caught by the byte-for-byte comparison
// below, not just a semantic json.Equal.
//
// preview_archives_by_default is intentionally ABSENT from this map: v3 never
// shipped that key at all (it's a pure v4 addition — the one known gap in
// the v3→v4 preview-management port), so a real v3 source dump would never
// contain it either — that gap is exercised
// separately in TestMigrateV4PreviewArchivesDefaultNotWrittenBySourcelessMigration
// below (migratev4's own side) and in
// internal/bootstrap/data's TestInitSettingsBackfillsMissingKeyWithoutClobberingCustomizedOnes
// (the boot-time backfill that actually closes the gap).
var previewSettingsFixture = map[string]string{
	conf.TextTypes:           "txt,htm,html,xml,java,properties,sql,js,md,json,conf,ini,vue,php,py,bat,gitignore,yml,go,sh,c,cpp,h,hpp,tsx,vtt,srt,ass,rs,lrc,rst",
	conf.AudioTypes:          "mp3,flac,ogg,m4a,wav,opus,wma,ape",
	conf.VideoTypes:          "mp4,mkv,avi,mov,rmvb,webm,flv,m3u8,ts",
	conf.ImageTypes:          "jpg,tiff,jpeg,png,gif,bmp,svg,ico,swf,webp,avif",
	conf.ProxyTypes:          "m3u8,url,ts",
	conf.ProxyIgnoreHeaders:  "authorization,referer,cookie",
	conf.AudioAutoplay:       "false",
	conf.VideoAutoplay:       "false",
	conf.ThumbnailSize:       "256",
	conf.ReadMeAutoRender:    "false",
	conf.FilterReadMeScripts: "false",
	"audio_cover":            "https://static.example.com/custom-cover.png",
	"external_previews": `{
  "torrent": {
    "BitTorrent Web": "https://example.com/torrent-open?u=$e_durl"
  }
}`,
	"iframe_previews": `{
  "doc,docx,xls,xlsx,ppt,pptx": {
    "Microsoft": "https://view.officeapps.live.com/op/view.aspx?src=$e_url",
    "Google": "https://docs.google.com/gview?url=$e_url&embedded=true"
  },
  "pdf": {
    "PDF.js": "https://alist-org.github.io/pdf.js/web/viewer.html?file=$e_url"
  },
  "/\\.(heic|raw)$/i": {
    "HEIC/RAW Viewer": "https://example.com/heic-viewer?src=$e_url"
  }
}`,
	"preview_settings": `{
  "pdf": {
    "order": ["builtin:pdf", "iframe:PDF.js", "iframe:Google"],
    "disabled": ["iframe:Google"]
  }
}`,
}

func TestPreviewSettingsFixtureIsValidJSON(t *testing.T) {
	for _, key := range []string{"external_previews", "iframe_previews", "preview_settings"} {
		if !json.Valid([]byte(previewSettingsFixture[key])) {
			t.Fatalf("fixture bug: %s is not valid JSON:\n%s", key, previewSettingsFixture[key])
		}
	}
}

// writeV3PreviewFixture builds a minimal-but-real-shaped v3 sqlite source (one
// admin user, one known-driver storage, config.json) whose x_setting_items
// table carries every key in previewSettingsFixture, verbatim.
func writeV3PreviewFixture(t *testing.T, dir, knownDriver string) {
	t.Helper()
	dbPath := filepath.Join(dir, "data.db")
	src, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	if err := src.AutoMigrate(&fxUser{}, &fxStorage{}, &fxSetting{}); err != nil {
		t.Fatalf("migrate fixture: %v", err)
	}

	adminSalt := "wsalt00000000000"
	if err := src.Create(&fxUser{
		ID: 1, Username: "admin", Salt: adminSalt,
		PwdHash: model.TwoHashPwd("adminpass", adminSalt),
		PwdTS:   111, BasePath: "/", Role: model.ADMIN, Permission: 0xFFFF,
	}).Error; err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	if err := src.Create(&fxStorage{
		ID: 1, MountPath: "/local", Driver: knownDriver, Status: "work", Addition: "{}",
	}).Error; err != nil {
		t.Fatalf("seed storage: %v", err)
	}

	settings := make([]fxSetting, 0, len(previewSettingsFixture))
	for k, v := range previewSettingsFixture {
		settings = append(settings, fxSetting{Key: k, Value: v})
	}
	if err := src.Create(&settings).Error; err != nil {
		t.Fatalf("seed preview settings: %v", err)
	}

	cfg := `{"database":{"type":"sqlite3","db_file":"data.db","table_prefix":"x_"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	if sqlDB, err := src.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// TestMigrateV4CarriesPreviewSettingsVerbatim is the W4 (迁移体验收尾) headline
// assertion for the v3 预览管理 port: a real-shaped v3 source dump customizing
// all of the preview-management keys must land on v4 byte-for-byte, and none of
// them may show up in the migration report's "dropped, no v4 equivalent" list
// (spec §4: they are all already registered in data.InitialSettings(), so the
// generic by-key-name migratev4 logic already carries them — this test is the
// proof, not a rewrite of that logic).
func TestMigrateV4CarriesPreviewSettingsVerbatim(t *testing.T) {
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}
	knownDriver := firstDriverName(t)

	root := t.TempDir()
	sourceDir := filepath.Join(root, "v3")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	writeV3PreviewFixture(t, sourceDir, knownDriver)

	target := filepath.Join(root, "v4")
	report, err := Run(Options{Source: sourceDir, Target: target})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for key, want := range previewSettingsFixture {
		if contains(report.SettingsDropped, key) {
			t.Errorf("setting %q must not be reported dropped (it is registered in InitialSettings()): dropped=%v", key, report.SettingsDropped)
		}
		got, err := db.GetSettingItemByKey(key)
		if err != nil {
			t.Fatalf("get carried setting %q: %v", key, err)
		}
		if got.Value != want {
			t.Errorf("setting %q not carried byte-identical:\n got:  %q\n want: %q", key, got.Value, want)
		}
	}
}

// TestMigrateV4PreviewArchivesDefaultNotWrittenBySourcelessMigration exercises
// the §4 "唯一缺口" from migratev4's own side: preview_archives_by_default did
// not exist in v3 at all, so a real v3 source dump never has it — migratev4
// (which only ever carries keys that exist in the source) has nothing to carry
// for that key, and must not report it dropped either (there is nothing to
// drop: "dropped" only applies to source keys v4 doesn't recognise, and this
// key is never even present in the source in the first place).
//
// migratev4 is explicitly a one-shot, source-to-fresh-target action (see the
// package doc comment) — it never touches the target again afterwards, so it
// is NOT the mechanism that back-fills a key added to v4 after an instance
// already migrated. Per docs/UPGRADE.md, the step right after `alist migrate
// v4` is starting the v4 server against the freshly created data dir, which
// runs the normal boot sequence instead (internal/bootstrap/data.InitData ->
// initSettings). That backfill-without-clobbering behaviour is proven directly
// against initSettings in internal/bootstrap/data (see
// TestInitSettingsBackfillsMissingKeyWithoutClobberingCustomizedOnes) rather
// than re-driven through a second migratev4-package Run() here — that used to
// be a reason to avoid a second Run() in this file at all: op's process-wide
// caches (internal/op/role.go's roleCache chief among them) went stale across
// an in-process db handle swap (migrate database A -> boot -> migrate
// database B -> boot would resolve B's roles against A's leftover cache
// entries). That's fixed now — see db.RegisterOnHandleChanged /
// internal/op/process_cache.go — and the exact cross-database sequence is
// covered as a regression by TestChainedMigrateThenBootAcrossDatabases in
// rolecache_process_test.go (verified to fail without the fix). This file's
// TestMigrateThenBootRolesAndSettingsCorrect, also in
// rolecache_process_test.go, is a separate, single-database migrate+boot
// integration test (settings carried byte-identical, built-in roles
// self-healed, admin bound correctly) — it exercises only one database, so it
// gives no coverage of the roleCache cross-database regression above; don't
// read it as a second witness for that fix.
func TestMigrateV4PreviewArchivesDefaultNotWrittenBySourcelessMigration(t *testing.T) {
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}
	knownDriver := firstDriverName(t)

	root := t.TempDir()
	sourceDir := filepath.Join(root, "v3")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	// A real v3 dump: preview_archives_by_default is not among the seeded keys
	// (writeV3PreviewFixture only ever seeds previewSettingsFixture, which
	// deliberately omits it — see the fixture's doc comment).
	writeV3PreviewFixture(t, sourceDir, knownDriver)

	target := filepath.Join(root, "v4")
	report, err := Run(Options{Source: sourceDir, Target: target})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if contains(report.SettingsDropped, conf.PreviewArchivesByDefault) {
		t.Errorf("preview_archives_by_default absent from the v3 source is not a 'dropped' setting (nothing to drop): dropped=%v", report.SettingsDropped)
	}
	// Immediately after Run(), the process's global conf/db already point at
	// the freshly migrated target (initTarget rewires them).
	if _, err := db.GetSettingItemByKey(conf.PreviewArchivesByDefault); err == nil {
		t.Fatalf("%s should not exist yet — migratev4 alone never wrote it (source had none); the boot-time backfill is a separate mechanism", conf.PreviewArchivesByDefault)
	}
}
