package flags

var (
	DataDir     string
	Debug       bool
	NoPrefix    bool
	Dev         bool
	ForceBinDir bool
	LogStd      bool

	// MigrateMode picks exactly one startup schema path (rollout Step 1):
	//   auto   - run pending migrations on startup (default)
	//   off    - never touch the schema; admin migrates via `alist migrate`
	//   legacy - old gorm AutoMigrate safety valve; refuses to start once any
	//            post-baseline migration has been applied
	MigrateMode string
)
