package migratev4

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/migratev4/legacy"
	"github.com/alist-org/alist/v3/pkg/utils"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// source is the in-memory read-only snapshot of a v3 database. The whole
// database is read up front and the connection closed before the target is
// touched, so the global db handle is never pointed at the source.
type source struct {
	prefix   string
	settings []legacy.SettingItem
	users    []legacy.User
	roles    []legacy.Role
	storages []legacy.Storage
	metas    []legacy.Meta

	hasRolesTable bool
	taskCount     int64
	// dropped maps a dropped-domain label to its row count in the source.
	dropped map[string]int64
}

// droppedDomains lists the v3 tables v4 dropped (plugin-era domains). The match
// key is the de-prefixed, lower-cased source table name; SearchNode used a
// literal (un-prefixed, mixed-case) table name in v3, handled by the lower-case
// compare.
var droppedDomains = map[string]string{
	"shares":              "shares",
	"labels":              "labels",
	"label_file_bindings": "label_file_bindings",
	"obj_files":           "obj_files",
	"search_nodes":        "search_nodes",
	"searchnode":          "search_nodes",
	"ssh_public_keys":     "ssh_public_keys",
	"s_sh_public_keys":    "ssh_public_keys",
}

// openSource resolves the --source argument (a v3 data dir containing
// config.json, or a bare sqlite .db file) into a read-only gorm handle plus the
// effective table prefix.
func openSource(srcArg, prefixOverride string) (*gorm.DB, string, error) {
	silent := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}

	info, err := os.Stat(srcArg)
	if err != nil {
		return nil, "", fmt.Errorf("source %q: %w", srcArg, err)
	}

	if info.IsDir() {
		cfgPath := filepath.Join(srcArg, "config.json")
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			return nil, "", fmt.Errorf("reading v3 config %q: %w", cfgPath, err)
		}
		var parsed struct {
			Database conf.Database `json:"database"`
		}
		if err := utils.Json.Unmarshal(data, &parsed); err != nil {
			return nil, "", fmt.Errorf("parsing v3 config %q: %w", cfgPath, err)
		}
		dbCfg := parsed.Database
		prefix := dbCfg.TablePrefix
		if prefixOverride != "" {
			prefix = prefixOverride
		}
		cfg := gormConfigWithPrefix(silent, prefix)
		db, err := openByDatabaseConfig(dbCfg, srcArg, cfg)
		return db, prefix, err
	}

	// Bare file: treat as a sqlite database.
	prefix := prefixOverride
	if prefix == "" {
		prefix = "x_" // v3 default table prefix
	}
	db, err := openReadOnlySqlite(srcArg, gormConfigWithPrefix(silent, prefix))
	return db, prefix, err
}

func gormConfigWithPrefix(base *gorm.Config, prefix string) *gorm.Config {
	cfg := *base
	cfg.NamingStrategy = schema.NamingStrategy{TablePrefix: prefix}
	return &cfg
}

func openByDatabaseConfig(dbCfg conf.Database, dir string, cfg *gorm.Config) (*gorm.DB, error) {
	switch dbCfg.Type {
	case "", "sqlite3":
		path := dbCfg.DBFile
		if path == "" {
			path = filepath.Join(dir, "data.db")
		} else if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		return openReadOnlySqlite(path, cfg)
	case "mysql":
		dsn := dbCfg.DSN
		if dsn == "" {
			dsn = fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local&tls=%s",
				dbCfg.User, dbCfg.Password, dbCfg.Host, dbCfg.Port, dbCfg.Name, dbCfg.SSLMode)
		}
		return gorm.Open(mysql.Open(dsn), cfg)
	case "postgres":
		dsn := dbCfg.DSN
		if dsn == "" {
			dsn = fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%d sslmode=%s",
				dbCfg.Host, dbCfg.User, dbCfg.Password, dbCfg.Name, dbCfg.Port, dbCfg.SSLMode)
		}
		return gorm.Open(postgres.Open(dsn), cfg)
	default:
		return nil, fmt.Errorf("unsupported v3 database type %q", dbCfg.Type)
	}
}

// openReadOnlySqlite opens a sqlite file in read-only mode: the tool must never
// mutate the source. immutable=1 also tolerates a missing -wal/-shm sidecar.
func openReadOnlySqlite(path string, cfg *gorm.Config) (*gorm.DB, error) {
	if !utils.Exists(path) {
		return nil, fmt.Errorf("sqlite database %q does not exist", path)
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1", path)
	return gorm.Open(sqlite.Open(dsn), cfg)
}

// readSource loads the whole v3 database into memory.
func readSource(srcArg, prefixOverride string) (*source, error) {
	db, prefix, err := openSource(srcArg, prefixOverride)
	if err != nil {
		return nil, err
	}
	// Ensure the underlying connection is released before the target opens.
	defer func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}()

	s := &source{prefix: prefix, dropped: map[string]int64{}}

	// Users are always present in a v3 database.
	if err := db.Find(&s.users).Error; err != nil {
		return nil, fmt.Errorf("reading v3 users: %w", err)
	}
	// Settings, storages, metas: best-effort (a fresh v3 install may lack rows,
	// but the tables exist). Errors here are real read failures.
	if db.Migrator().HasTable(&legacy.SettingItem{}) {
		if err := db.Find(&s.settings).Error; err != nil {
			return nil, fmt.Errorf("reading v3 settings: %w", err)
		}
	}
	if db.Migrator().HasTable(&legacy.Storage{}) {
		if err := db.Find(&s.storages).Error; err != nil {
			return nil, fmt.Errorf("reading v3 storages: %w", err)
		}
	}
	if db.Migrator().HasTable(&legacy.Meta{}) {
		if err := db.Find(&s.metas).Error; err != nil {
			return nil, fmt.Errorf("reading v3 metas: %w", err)
		}
	}
	// Roles table exists from v3.46+; older databases carry role semantics in
	// the users.role int column alone.
	if db.Migrator().HasTable(&legacy.Role{}) {
		s.hasRolesTable = true
		if err := db.Find(&s.roles).Error; err != nil {
			return nil, fmt.Errorf("reading v3 roles: %w", err)
		}
	}

	countDroppedAndTasks(db, prefix, s)
	return s, nil
}

// countDroppedAndTasks enumerates the source's tables and tallies rows in the
// transient task table and the six dropped plugin-era domains. All best-effort:
// a missing table simply contributes zero.
func countDroppedAndTasks(db *gorm.DB, prefix string, s *source) {
	tables, err := db.Migrator().GetTables()
	if err != nil {
		return
	}
	for _, tbl := range tables {
		bare := strings.ToLower(strings.TrimPrefix(tbl, prefix))
		switch {
		case bare == "task_items" || bare == "tasks":
			var n int64
			if db.Table(tbl).Count(&n).Error == nil {
				s.taskCount += n
			}
		default:
			if label, ok := droppedDomains[bare]; ok {
				var n int64
				if db.Table(tbl).Count(&n).Error == nil {
					s.dropped[label] += n
				}
			}
		}
	}
}
