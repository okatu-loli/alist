package migration

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// driftGuardDBSeq gives each openMem call its own named in-memory database.
// Prevents test contamination via cache=shared state leakage.
var driftGuardDBSeq atomic.Int64

// dumpSchema 生成 "table.column" 全集（不比列类型——方言间类型名不可比，
// 列集合足以抓"改 model 不写迁移"）。
func dumpSchema(t *testing.T, d *gorm.DB) []string {
	t.Helper()
	tables, err := d.Migrator().GetTables()
	if err != nil {
		t.Fatalf("get tables: %v", err)
	}
	var out []string
	for _, tb := range tables {
		if strings.HasSuffix(tb, "schema_migrations") {
			continue
		}
		// Skip sqlite dialect system tables (e.g., sqlite_sequence).
		if strings.HasPrefix(tb, "sqlite_") {
			continue
		}
		cols, err := d.Migrator().ColumnTypes(tb)
		if err != nil {
			t.Fatalf("columns of %s: %v", tb, err)
		}
		for _, c := range cols {
			out = append(out, fmt.Sprintf("%s.%s", tb, c.Name()))
		}
		idxs, err := d.Migrator().GetIndexes(tb)
		if err != nil {
			t.Fatalf("indexes of %s: %v", tb, err)
		}
		for _, ix := range idxs {
			uniq, _ := ix.Unique()
			out = append(out, fmt.Sprintf("%s.index:%s(%s)unique=%v",
				tb, ix.Name(), strings.Join(ix.Columns(), ","), uniq))
		}
	}
	sort.Strings(out)
	return out
}

func openMem(t *testing.T, dsnTag string) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", dsnTag, driftGuardDBSeq.Add(1))
	d, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: testPrefix},
		Logger:         logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return d
}

// 若此测试变红：你改了 internal/model 的 7 张核心表却没写迁移。
// 修法是新增 NNNN_*.go 迁移，绝不是改 0000_baseline 的冻结快照。
func TestSchemaDriftGuard(t *testing.T) {
	viaMigrator := openMem(t, "guard_migrator")
	if err := New(viaMigrator, testPrefix).Up(); err != nil {
		t.Fatalf("migrator up: %v", err)
	}

	viaModels := openMem(t, "guard_models")
	if err := viaModels.AutoMigrate(
		&model.Storage{}, &model.User{}, &model.Meta{}, &model.SettingItem{},
		&model.TaskItem{}, &model.Role{}, &model.Session{},
		&model.Permission{}, &model.RolePermission{}, &model.UserRole{},
		&model.SigningKey{}, &model.Token{}, &model.AuditLog{}, &model.Node{},
		&model.Label{}, &model.LabelBinding{}, &model.Share{}, &model.AppPassword{},
		&model.Agent{}, &model.AgentTask{}, &model.SearchNode{}, &model.RecoveryCode{},
	); err != nil {
		t.Fatalf("automigrate live models: %v", err)
	}

	got, want := dumpSchema(t, viaMigrator), dumpSchema(t, viaModels)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("schema drift between migrations and live models.\n--- via migrations ---\n%s\n--- via live models ---\n%s\n(fix: add a new migration, do NOT edit 0000_baseline)",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// registry 约定：文件名 NNNN_<slug>.go、ID 与文件名一致，注册顺序即字典序。
// Down()/Status() 对"最后一条已应用迁移"的判定依赖这一约定。
func TestRegistryOrderMatchesIDOrder(t *testing.T) {
	for i := 1; i < len(registry); i++ {
		if registry[i-1].ID >= registry[i].ID {
			t.Errorf("registry order violates ID lexical order: %q (index %d) >= %q (index %d)",
				registry[i-1].ID, i-1, registry[i].ID, i)
		}
	}
}
