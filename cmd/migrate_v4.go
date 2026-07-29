package cmd

import (
	"fmt"

	"github.com/alist-org/alist/v3/internal/migratev4"
	"github.com/spf13/cobra"
)

var (
	migrateV4Source       string
	migrateV4SourcePrefix string
	migrateV4Target       string
	migrateV4DryRun       bool
)

// migrateV4Cmd is the v3→v4 data migration orchestrator (rollout Step 14). It
// reads a v3 database read-only and builds a fresh v4 data directory; it never
// modifies the source, so rollback is simply keeping the old data dir.
var migrateV4Cmd = &cobra.Command{
	Use:   "v4",
	Short: "Migrate a v3 database into a fresh v4 data directory",
	Long: `Migrate a v3 alist database into a new v4 data directory.

The source is read read-only and is never modified. The target must not already
contain a database. Run with --dry-run first to preview exactly what will be
carried, dropped, or imported disabled, plus the breaking changes v4 introduces.

Examples:
  alist migrate v4 --source /old/alist/data --dry-run
  alist migrate v4 --source /old/alist/data --target ./data-v4`,
	Run: func(cmd *cobra.Command, args []string) {
		report, err := migratev4.Run(migratev4.Options{
			Source:       migrateV4Source,
			SourcePrefix: migrateV4SourcePrefix,
			Target:       migrateV4Target,
			DryRun:       migrateV4DryRun,
		})
		if err != nil {
			fmt.Printf("migrate v4 failed: %+v\n", err)
			return
		}
		fmt.Print(report.String())
	},
}

func init() {
	migrateV4Cmd.Flags().StringVar(&migrateV4Source, "source", "", "v3 data directory (with config.json) or v3 sqlite .db file")
	migrateV4Cmd.Flags().StringVar(&migrateV4SourcePrefix, "source-prefix", "", "override the v3 table prefix (default: from config.json, or x_ for a bare .db)")
	migrateV4Cmd.Flags().StringVar(&migrateV4Target, "target", "", "v4 data directory to create (required unless --dry-run)")
	migrateV4Cmd.Flags().BoolVar(&migrateV4DryRun, "dry-run", false, "read and convert everything but write nothing")
	MigrateCmd.AddCommand(migrateV4Cmd)
}
