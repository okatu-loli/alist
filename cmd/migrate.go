package cmd

import (
	"fmt"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/spf13/cobra"
)

// migrateInit connects config/log/db WITHOUT running any schema path or data
// seeding — the subcommand itself decides what to do with the schema.
func migrateInit() *migration.Migrator {
	flags.MigrateMode = "off"
	bootstrap.InitConfig()
	bootstrap.Log()
	bootstrap.InitDB()
	return migration.New(db.GetDb(), conf.Conf.Database.TablePrefix)
}

var MigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Manage database schema migrations",
}

var migrateUpCmd = &cobra.Command{
	Use:   "up",
	Short: "Apply all pending migrations",
	Run: func(cmd *cobra.Command, args []string) {
		m := migrateInit()
		defer Release()
		if err := m.Up(); err != nil {
			utils.Log.Fatalf("migrate up failed: %+v", err)
		}
		utils.Log.Info("migrate up: done")
	},
}

var migrateDownCmd = &cobra.Command{
	Use:   "down",
	Short: "Roll back the most recent migration (stops at the baseline)",
	Run: func(cmd *cobra.Command, args []string) {
		m := migrateInit()
		defer Release()
		if err := m.Down(); err != nil {
			utils.Log.Fatalf("migrate down failed: %+v", err)
		}
		utils.Log.Info("migrate down: done")
	},
}

var migrateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show applied and pending migrations",
	Run: func(cmd *cobra.Command, args []string) {
		m := migrateInit()
		defer Release()
		applied, pending, err := m.Status()
		if err != nil {
			utils.Log.Fatalf("migrate status failed: %+v", err)
		}
		fmt.Println("applied:")
		if len(applied) == 0 {
			fmt.Println("  (none)")
		}
		for _, id := range applied {
			fmt.Printf("  %s\n", id)
		}
		fmt.Println("pending:")
		if len(pending) == 0 {
			fmt.Println("  (none)")
		}
		for _, id := range pending {
			fmt.Printf("  %s\n", id)
		}
	},
}

func init() {
	MigrateCmd.AddCommand(migrateUpCmd, migrateDownCmd, migrateStatusCmd)
	RootCmd.AddCommand(MigrateCmd)
}
