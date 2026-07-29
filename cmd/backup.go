package cmd

import (
	"fmt"
	"os"

	"github.com/alist-org/alist/v3/internal/backup"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/spf13/cobra"
)

// BackupCmd groups the export/import subcommands. They boot config+db exactly
// like `alist migrate` (no server, no stream limits) and reuse the same
// exporter/importer as the HTTP API so a host with no web UI can still take and
// restore backups for disaster recovery.
var BackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Export or import a backup of the configuration database",
}

var (
	backupOutput   string
	backupPassword string
	backupSanitize bool
	backupStrategy string
	backupDryRun   bool
)

var backupExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export settings/users/roles/permissions/metas/storages into a backup archive",
	Run: func(cmd *cobra.Command, args []string) {
		Init()
		defer Release()
		out := backupOutput
		if out == "" {
			// No --output: write into the standard backup dir like the API does.
			name, _, err := backup.Export(backup.Options{Password: backupPassword, Sanitize: backupSanitize})
			if err != nil {
				utils.Log.Fatalf("backup export failed: %+v", err)
			}
			fmt.Printf("wrote %s\n", name)
			return
		}
		arc, err := backup.Collect(db.GetDb())
		if err != nil {
			utils.Log.Fatalf("backup export failed: %+v", err)
		}
		f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			utils.Log.Fatalf("cannot create %s: %+v", out, err)
		}
		manifest, werr := backup.Write(f, arc, backup.Options{Password: backupPassword, Sanitize: backupSanitize})
		cerr := f.Close()
		if werr != nil {
			utils.Log.Fatalf("backup export failed: %+v", werr)
		}
		if cerr != nil {
			utils.Log.Fatalf("backup export failed: %+v", cerr)
		}
		fmt.Printf("wrote %s (schema %s, encrypted=%v, sanitized=%v)\n",
			out, manifest.SchemaVersion, manifest.Encrypted, manifest.Sanitized)
	},
}

var backupImportCmd = &cobra.Command{
	Use:   "import <file>",
	Short: "Restore a backup archive (use --dry-run to preview the change)",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		strategy := backupStrategy
		if strategy == "" {
			strategy = backup.StrategyMerge
		}
		if strategy != backup.StrategyMerge && strategy != backup.StrategyOverwrite {
			utils.Log.Fatalf("strategy must be %q or %q", backup.StrategyMerge, backup.StrategyOverwrite)
		}
		Init()
		defer Release()

		f, err := os.Open(args[0])
		if err != nil {
			utils.Log.Fatalf("cannot open %s: %+v", args[0], err)
		}
		arc, err := backup.Read(f, backupPassword)
		_ = f.Close()
		if err != nil {
			utils.Log.Fatalf("backup import failed: %+v", err)
		}
		diff, err := backup.Apply(arc, strategy, backupDryRun)
		if err != nil {
			utils.Log.Fatalf("backup import failed: %+v", err)
		}
		verb := "applied"
		if backupDryRun {
			verb = "would apply (dry-run)"
		}
		fmt.Printf("%s (%s):\n", verb, strategy)
		for _, d := range []string{"settings", "storages", "metas", "roles", "permissions", "users", "user_roles", "role_permissions"} {
			dd := diff.Domains[d]
			fmt.Printf("  %-17s create=%d update=%d delete=%d\n", d, len(dd.Create), len(dd.Update), len(dd.Delete))
		}
	},
}

func init() {
	backupExportCmd.Flags().StringVarP(&backupOutput, "output", "o", "", "write archive to this path instead of the data-dir backup folder")
	backupExportCmd.Flags().StringVar(&backupPassword, "password", "", "encrypt domain payloads with this password (AES-256-GCM)")
	backupExportCmd.Flags().BoolVar(&backupSanitize, "sanitize", false, "strip storage credentials and user secrets (mutually exclusive with --password)")

	backupImportCmd.Flags().StringVar(&backupPassword, "password", "", "password for an encrypted archive")
	backupImportCmd.Flags().StringVar(&backupStrategy, "strategy", backup.StrategyMerge, "merge | overwrite")
	backupImportCmd.Flags().BoolVar(&backupDryRun, "dry-run", false, "preview the change without writing")

	BackupCmd.AddCommand(backupExportCmd, backupImportCmd)
	RootCmd.AddCommand(BackupCmd)
}
