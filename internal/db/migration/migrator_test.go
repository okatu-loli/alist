package migration

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

const testPrefix = "x_"

// sqliteDBSeq gives every openTestDBs call its own named in-memory database.
// A shared literal DSN ("file::memory:?cache=shared") would have every test
// in this binary reconnect to the SAME database (cache=shared keeps it alive
// across gorm.Open calls), including sqlite's internal sqlite_sequence table
// which wipe() cannot drop — leaking state and failing later tests.
var sqliteDBSeq atomic.Int64

// openTestDBs 返回可用的测试库：sqlite 恒有；mysql/postgres 由环境变量注入，缺则跳过。
// 每个库在返回前都被清空（删除全部表），保证用例隔离。
func openTestDBs(t *testing.T) map[string]*gorm.DB {
	t.Helper()
	cfg := &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: testPrefix},
		Logger:         logger.Default.LogMode(logger.Silent),
	}
	dbs := map[string]*gorm.DB{}

	dsn := fmt.Sprintf("file:migrationtest%d?mode=memory&cache=shared&_pragma=foreign_keys(1)", sqliteDBSeq.Add(1))
	sq, err := gorm.Open(sqlite.Open(dsn), cfg)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	dbs["sqlite"] = sq

	if dsn := os.Getenv("TEST_MYSQL_DSN"); dsn != "" {
		d, err := gorm.Open(mysql.Open(dsn), cfg)
		if err != nil {
			t.Fatalf("open mysql: %v", err)
		}
		dbs["mysql"] = d
	}
	if dsn := os.Getenv("TEST_PG_DSN"); dsn != "" {
		d, err := gorm.Open(postgres.Open(dsn), cfg)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		dbs["postgres"] = d
	}

	for name, d := range dbs {
		wipe(t, name, d)
	}
	return dbs
}

func wipe(t *testing.T, name string, d *gorm.DB) {
	t.Helper()
	tables, err := d.Migrator().GetTables()
	if err != nil {
		t.Fatalf("[%s] list tables: %v", name, err)
	}
	for _, tb := range tables {
		if err := d.Migrator().DropTable(tb); err != nil {
			t.Fatalf("[%s] drop %s: %v", name, tb, err)
		}
	}
}

func TestMigratorUpOnEmptyDB(t *testing.T) {
	for name, d := range openTestDBs(t) {
		m := New(d, testPrefix)
		if err := m.Up(); err != nil {
			t.Fatalf("[%s] Up: %v", name, err)
		}
		if !d.Migrator().HasTable(m.TableName()) {
			t.Errorf("[%s] migration table %s not created", name, m.TableName())
		}
	}
}

func coreTables() []string {
	return []string{
		testPrefix + "storages", testPrefix + "users", testPrefix + "meta",
		testPrefix + "setting_items", testPrefix + "task_items",
		testPrefix + "roles", testPrefix + "sessions",
	}
}

// ① 空库：建出 7 张核心表并记录 baseline。
func TestBaselineFreshDB(t *testing.T) {
	for name, d := range openTestDBs(t) {
		m := New(d, testPrefix)
		if err := m.Up(); err != nil {
			t.Fatalf("[%s] Up: %v", name, err)
		}
		for _, tb := range coreTables() {
			if !d.Migrator().HasTable(tb) {
				t.Errorf("[%s] missing core table %s", name, tb)
			}
		}
		var n int64
		d.Table(m.TableName()).Where("id = ?", "0000_baseline").Count(&n)
		if n != 1 {
			t.Errorf("[%s] baseline not recorded, got %d rows", name, n)
		}
	}
}

// ② 老库（缺列 + 含遗留表）：先拉平再标记；遗留表原样保留。
func TestBaselineLevelsUpLegacyDB(t *testing.T) {
	for name, d := range openTestDBs(t) {
		// 造老库：先建基线表，再删掉一列模拟旧版本，再造一张遗留表
		if err := d.AutoMigrate(baselineModels()...); err != nil {
			t.Fatalf("[%s] seed legacy db: %v", name, err)
		}
		if err := d.Migrator().DropColumn(&User{}, "otp_secret"); err != nil {
			t.Fatalf("[%s] drop column: %v", name, err)
		}
		if err := d.Exec("CREATE TABLE " + testPrefix + "labels (id INTEGER PRIMARY KEY, name VARCHAR(64))").Error; err != nil {
			t.Fatalf("[%s] create legacy table: %v", name, err)
		}

		m := New(d, testPrefix)
		if err := m.Up(); err != nil {
			t.Fatalf("[%s] Up on legacy db: %v", name, err)
		}
		if !d.Migrator().HasColumn(&User{}, "otp_secret") {
			t.Errorf("[%s] legacy db not leveled up: users.otp_secret still missing", name)
		}
		if !d.Migrator().HasTable(testPrefix + "labels") {
			t.Errorf("[%s] legacy table was touched/dropped — must be left alone", name)
		}
		var n int64
		d.Table(m.TableName()).Where("id = ?", "0000_baseline").Count(&n)
		if n != 1 {
			t.Errorf("[%s] baseline not recorded on legacy db", name)
		}
	}
}

// ③ 幂等：重复 Up 不报错、不重复记录。
func TestBaselineIdempotent(t *testing.T) {
	for name, d := range openTestDBs(t) {
		m := New(d, testPrefix)
		if err := m.Up(); err != nil {
			t.Fatalf("[%s] first Up: %v", name, err)
		}
		if err := m.Up(); err != nil {
			t.Fatalf("[%s] second Up: %v", name, err)
		}
		var n int64
		d.Table(m.TableName()).Count(&n)
		if n != int64(len(registry)) {
			t.Errorf("[%s] expect exactly %d migration rows, got %d", name, len(registry), n)
		}
	}
}

// down 到 baseline 必须被拒绝（先剥掉 baseline 之后的所有迁移）。
func TestDownRefusesBaseline(t *testing.T) {
	for name, d := range openTestDBs(t) {
		m := New(d, testPrefix)
		if err := m.Up(); err != nil {
			t.Fatalf("[%s] Up: %v", name, err)
		}
		for i := len(registry) - 1; i > 0; i-- {
			if err := m.Down(); err != nil {
				t.Fatalf("[%s] down past %s: %v", name, registry[i].ID, err)
			}
		}
		err := m.Down()
		if !errors.Is(err, ErrBaselineIrreversible) {
			t.Errorf("[%s] expect ErrBaselineIrreversible, got %v", name, err)
		}
		if !d.Migrator().HasTable(testPrefix + "users") {
			t.Errorf("[%s] baseline tables must survive a refused down", name)
		}
	}
}

// 普通迁移 up→down→up 幂等（用临时注册的 dummy 迁移验证，不污染全局 registry）。
func TestUpDownUpRoundTrip(t *testing.T) {
	dummy := &gormigrate.Migration{
		ID: "0001_test_dummy",
		Migrate: func(tx *gorm.DB) error {
			return tx.Exec("CREATE TABLE " + testPrefix + "dummy (id INTEGER PRIMARY KEY)").Error
		},
		Rollback: func(tx *gorm.DB) error {
			return tx.Migrator().DropTable(testPrefix + "dummy")
		},
	}
	registry = append(registry, dummy)
	t.Cleanup(func() { registry = registry[:len(registry)-1] })

	for name, d := range openTestDBs(t) {
		m := New(d, testPrefix)
		if err := m.Up(); err != nil {
			t.Fatalf("[%s] up#1: %v", name, err)
		}
		if !d.Migrator().HasTable(testPrefix + "dummy") {
			t.Fatalf("[%s] dummy table missing after up", name)
		}
		if err := m.Down(); err != nil {
			t.Fatalf("[%s] down: %v", name, err)
		}
		if d.Migrator().HasTable(testPrefix + "dummy") {
			t.Fatalf("[%s] dummy table survived down", name)
		}
		if err := m.Up(); err != nil {
			t.Fatalf("[%s] up#2: %v", name, err)
		}
		if !d.Migrator().HasTable(testPrefix + "dummy") {
			t.Fatalf("[%s] dummy table missing after re-up", name)
		}
	}
}

// legacy 安全阀：一旦应用过 baseline 之外的迁移，legacy AutoMigrate 拒绝。
func TestGuardLegacyAutoMigrate(t *testing.T) {
	for name, d := range openTestDBs(t) {
		// 空库 / 仅 baseline：放行
		if err := GuardLegacyAutoMigrate(d, testPrefix); err != nil {
			t.Fatalf("[%s] guard should pass on empty db: %v", name, err)
		}
		m := New(d, testPrefix)
		if err := m.UpTo("0000_baseline"); err != nil {
			t.Fatalf("[%s] UpTo baseline: %v", name, err)
		}
		if err := GuardLegacyAutoMigrate(d, testPrefix); err != nil {
			t.Fatalf("[%s] guard should pass with baseline only: %v", name, err)
		}
		// 有后续迁移记录：拒绝
		if err := m.Up(); err != nil {
			t.Fatalf("[%s] Up: %v", name, err)
		}
		if err := GuardLegacyAutoMigrate(d, testPrefix); err == nil {
			t.Errorf("[%s] guard must refuse once post-baseline migrations exist", name)
		}
	}
}

// 0001 回填：挂载点内的 meta 落锚；虚拟路径 meta 锚置空且不报错；幂等。
func TestMetaAnchorBackfill(t *testing.T) {
	for name, d := range openTestDBs(t) {
		// 先到 baseline（老库形态：无锚列）
		mig := New(d, testPrefix)
		if err := mig.UpTo("0000_baseline"); err != nil {
			t.Fatalf("[%s] to baseline: %v", name, err)
		}
		// 种数据：一个存储 + 三条 meta（挂载内、挂载根、虚拟路径）
		if err := d.Exec("INSERT INTO " + testPrefix + "storages (id, mount_path, driver) VALUES (7, '/res', 'Local')").Error; err != nil {
			t.Fatalf("[%s] seed storage: %v", name, err)
		}
		for _, p := range []string{"/res/private", "/res", "/"} {
			if err := d.Exec("INSERT INTO "+testPrefix+"meta (path) VALUES (?)", p).Error; err != nil {
				t.Fatalf("[%s] seed meta %s: %v", name, p, err)
			}
		}
		// Stop at 0001: this test asserts the 0001 anchor backfill, and 0009
		// later drops those very columns (they moved to the nodes registry), so
		// a full Up would leave nothing to read.
		if err := mig.UpTo("0001_meta_anchor"); err != nil {
			t.Fatalf("[%s] up to 0001: %v", name, err)
		}
		type row struct {
			Path      string
			StorageID uint
			Anchor    string
		}
		var rows []row
		if err := d.Table(testPrefix + "meta").Order("path").Scan(&rows).Error; err != nil {
			t.Fatalf("[%s] scan: %v", name, err)
		}
		want := map[string]row{
			"/res/private": {StorageID: 7, Anchor: "/private"},
			"/res":         {StorageID: 7, Anchor: "/"},
			"/":            {StorageID: 0, Anchor: ""}, // 虚拟路径：锚置空，合法
		}
		for _, r := range rows {
			w := want[r.Path]
			if r.StorageID != w.StorageID || r.Anchor != w.Anchor {
				t.Errorf("[%s] meta %q backfill = {sid:%d anchor:%q}, want {sid:%d anchor:%q}",
					name, r.Path, r.StorageID, r.Anchor, w.StorageID, w.Anchor)
			}
		}
		// 幂等：重跑到 0001 无副作用
		if err := mig.UpTo("0001_meta_anchor"); err != nil {
			t.Fatalf("[%s] re-up: %v", name, err)
		}
	}
}

func TestStatus(t *testing.T) {
	for name, d := range openTestDBs(t) {
		m := New(d, testPrefix)
		applied, pending, err := m.Status()
		if err != nil {
			t.Fatalf("[%s] status before up: %v", name, err)
		}
		if len(applied) != 0 || len(pending) != len(registry) {
			t.Errorf("[%s] pre-up status wrong: applied=%v pending=%v", name, applied, pending)
		}
		if err := m.Up(); err != nil {
			t.Fatalf("[%s] Up: %v", name, err)
		}
		applied, pending, err = m.Status()
		if err != nil {
			t.Fatalf("[%s] status after up: %v", name, err)
		}
		if len(applied) != len(registry) || len(pending) != 0 {
			t.Errorf("[%s] post-up status wrong: applied=%v pending=%v", name, applied, pending)
		}
	}
}
