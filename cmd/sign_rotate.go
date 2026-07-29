package cmd

import (
	"github.com/alist-org/alist/v3/internal/sign"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/spf13/cobra"
)

// signCmd represents the sign command
var signCmd = &cobra.Command{
	Use:   "sign",
	Short: "Manage signing keys",
}

var signRotateCmd = &cobra.Command{
	Use:   "rotate <purpose>",
	Short: "Rotate the active signing key for a purpose, keeping the old key valid for a grace period",
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) < 1 {
			utils.Log.Errorf("purpose is required")
			return
		}
		purpose := args[0]
		Init()
		defer Release()
		kid, err := sign.Rotate(purpose)
		if err != nil {
			utils.Log.Errorf("failed to rotate signing key: %+v", err)
			return
		}
		utils.Log.Infof("Rotated %q signing key, new active kid: %s", purpose, kid)
		utils.Log.Warnf("Note: a running alist server caches signing keys in memory. To apply this change to a live server, restart it, or call the admin API (POST /api/admin/signing_key/rotate) on the running instance instead.")
	},
}

var signGCCmd = &cobra.Command{
	Use:   "gc <purpose>",
	Short: "Remove every non-active signing key for a purpose, ending their grace period",
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) < 1 {
			utils.Log.Errorf("purpose is required")
			return
		}
		purpose := args[0]
		Init()
		defer Release()
		removed, err := sign.GC(purpose)
		if err != nil {
			utils.Log.Errorf("failed to gc signing keys: %+v", err)
			return
		}
		utils.Log.Infof("Removed %d non-active %q signing key(s)", removed, purpose)
		utils.Log.Warnf("Note: a running alist server caches signing keys in memory. To apply this change to a live server, restart it, or call the admin API (POST /api/admin/signing_key/gc) on the running instance instead.")
	},
}

func init() {
	RootCmd.AddCommand(signCmd)
	signCmd.AddCommand(signRotateCmd)
	signCmd.AddCommand(signGCCmd)
}
