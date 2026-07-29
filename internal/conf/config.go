package conf

import (
	"path/filepath"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/pkg/utils/random"
)

type Database struct {
	Type        string `json:"type" env:"TYPE"`
	Host        string `json:"host" env:"HOST"`
	Port        int    `json:"port" env:"PORT"`
	User        string `json:"user" env:"USER"`
	Password    string `json:"password" env:"PASS"`
	Name        string `json:"name" env:"NAME"`
	DBFile      string `json:"db_file" env:"FILE"`
	TablePrefix string `json:"table_prefix" env:"TABLE_PREFIX"`
	SSLMode     string `json:"ssl_mode" env:"SSL_MODE"`
	DSN         string `json:"dsn" env:"DSN"`
}

type Scheme struct {
	Address      string `json:"address" env:"ADDR"`
	HttpPort     int    `json:"http_port" env:"HTTP_PORT"`
	HttpsPort    int    `json:"https_port" env:"HTTPS_PORT"`
	ForceHttps   bool   `json:"force_https" env:"FORCE_HTTPS"`
	CertFile     string `json:"cert_file" env:"CERT_FILE"`
	KeyFile      string `json:"key_file" env:"KEY_FILE"`
	UnixFile     string `json:"unix_file" env:"UNIX_FILE"`
	UnixFilePerm string `json:"unix_file_perm" env:"UNIX_FILE_PERM"`
	EnableH2c    bool   `json:"enable_h2c" env:"ENABLE_H2C"`
}

type LogConfig struct {
	Enable     bool   `json:"enable" env:"LOG_ENABLE"`
	Name       string `json:"name" env:"LOG_NAME"`
	MaxSize    int    `json:"max_size" env:"MAX_SIZE"`
	MaxBackups int    `json:"max_backups" env:"MAX_BACKUPS"`
	MaxAge     int    `json:"max_age" env:"MAX_AGE"`
	Compress   bool   `json:"compress" env:"COMPRESS"`
	// Lang selects the locale for operator-facing (OPS) lifecycle/error logs
	// (see internal/i18n.Log). Default "en" keeps English log output.
	Lang string `json:"lang" env:"LOG_LANG"`
}

type TaskConfig struct {
	Workers        int  `json:"workers" env:"WORKERS"`
	MaxRetry       int  `json:"max_retry" env:"MAX_RETRY"`
	TaskPersistant bool `json:"task_persistant" env:"TASK_PERSISTANT"`
}

type TasksConfig struct {
	Upload             TaskConfig `json:"upload" envPrefix:"UPLOAD_"`
	Copy               TaskConfig `json:"copy" envPrefix:"COPY_"`
	Download           TaskConfig `json:"download" envPrefix:"DOWNLOAD_"`
	AllowRetryCanceled bool       `json:"allow_retry_canceled" env:"ALLOW_RETRY_CANCELED"`
}

type Cors struct {
	AllowOrigins []string `json:"allow_origins" env:"ALLOW_ORIGINS"`
	AllowMethods []string `json:"allow_methods" env:"ALLOW_METHODS"`
	AllowHeaders []string `json:"allow_headers" env:"ALLOW_HEADERS"`
}

type Config struct {
	Force                 bool        `json:"force" env:"FORCE"`
	SiteURL               string      `json:"site_url" env:"SITE_URL"`
	Cdn                   string      `json:"cdn" env:"CDN"`
	JwtSecret             string      `json:"jwt_secret" env:"JWT_SECRET"`
	TokenExpiresIn        int         `json:"token_expires_in" env:"TOKEN_EXPIRES_IN"`
	Database              Database    `json:"database" envPrefix:"DB_"`
	Scheme                Scheme      `json:"scheme"`
	TempDir               string      `json:"temp_dir" env:"TEMP_DIR"`
	DistDir               string      `json:"dist_dir"`
	Log                   LogConfig   `json:"log"`
	DelayedStart          int         `json:"delayed_start" env:"DELAYED_START"`
	MaxConnections        int         `json:"max_connections" env:"MAX_CONNECTIONS"`
	MaxConcurrency        int         `json:"max_concurrency" env:"MAX_CONCURRENCY"`
	TlsInsecureSkipVerify bool        `json:"tls_insecure_skip_verify" env:"TLS_INSECURE_SKIP_VERIFY"`
	Tasks                 TasksConfig `json:"tasks" envPrefix:"TASKS_"`
	Cors                  Cors        `json:"cors" envPrefix:"CORS_"`
	LastLaunchedVersion   string      `json:"last_launched_version"`
	// BleveDir is the on-disk directory the bleve SearchIndexer backend stores
	// its index under (子项目 A, wired in A5). Defaults to <data>/bleve.
	BleveDir string `json:"bleve_dir" env:"BLEVE_DIR"`
	// Meilisearch configures the external meilisearch SearchIndexer backend
	// (子项目 A, wired in A6). Declared now so the config-file contract is stable.
	Meilisearch Meilisearch `json:"meilisearch" envPrefix:"MEILISEARCH_"`
}

// Meilisearch is the connection config for the meilisearch SearchIndexer
// backend. IndexPrefix namespaces this instance's indexes on a shared server.
type Meilisearch struct {
	Host        string `json:"host" env:"HOST"`
	APIKey      string `json:"api_key" env:"API_KEY"`
	IndexPrefix string `json:"index_prefix" env:"INDEX_PREFIX"`
}

func DefaultConfig() *Config {
	tempDir := filepath.Join(flags.DataDir, "temp")
	logPath := filepath.Join(flags.DataDir, "log/log.log")
	dbPath := filepath.Join(flags.DataDir, "data.db")
	bleveDir := filepath.Join(flags.DataDir, "bleve")
	return &Config{
		Scheme: Scheme{
			Address:    "0.0.0.0",
			UnixFile:   "",
			HttpPort:   5244,
			HttpsPort:  -1,
			ForceHttps: false,
			CertFile:   "",
			KeyFile:    "",
		},
		JwtSecret:      random.String(16),
		TokenExpiresIn: 48,
		TempDir:        tempDir,
		Database: Database{
			Type:        "sqlite3",
			Port:        0,
			TablePrefix: "x_",
			DBFile:      dbPath,
		},
		Log: LogConfig{
			Enable:     true,
			Name:       logPath,
			MaxSize:    50,
			MaxBackups: 30,
			MaxAge:     28,
			Lang:       "en",
		},
		MaxConnections:        0,
		MaxConcurrency:        64,
		TlsInsecureSkipVerify: false,
		Tasks: TasksConfig{
			Upload: TaskConfig{
				Workers:        5,
				TaskPersistant: true,
			},
			Copy: TaskConfig{
				Workers:        5,
				MaxRetry:       2,
				TaskPersistant: true,
			},
			// Offline downloads re-fetch from the source URL on resume (no
			// on-disk temp file to relocate like uploads), so a modest retry
			// budget covers transient network/redirect failures. Persistence
			// keeps queued/in-flight downloads across a restart since the whole
			// task state is JSON-serializable (URL + destination).
			Download: TaskConfig{
				Workers:        3,
				MaxRetry:       2,
				TaskPersistant: true,
			},
			AllowRetryCanceled: false,
		},
		Cors: Cors{
			AllowOrigins: []string{"*"},
			AllowMethods: []string{"*"},
			AllowHeaders: []string{"*"},
		},
		LastLaunchedVersion: "",
		BleveDir:            bleveDir,
		Meilisearch: Meilisearch{
			Host: "http://localhost:7700",
		},
	}
}
