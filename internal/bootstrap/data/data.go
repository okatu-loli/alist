package data

import "github.com/alist-org/alist/v3/cmd/flags"

func InitData() {
	initRoles()
	// Seed the built-in operator presets and backfill admin's system rows AFTER
	// initRoles (admin/guest exist) and never in a migration (spec §10). Both
	// are row-level idempotent, so re-running bootstrap converges without dups.
	initPresetRoles()
	backfillAdminSystem()
	healRoleScopeBindings()
	isNewInstall := initUser()
	initSettings()
	if isNewInstall {
		// Must run after initSettings has seeded conf.SetupCompleted="true";
		// this flips it to "false" so the first-run setup wizard is shown.
		markFreshInstall()
	}
	initTasks()
	if flags.Dev {
		initDevData()
	}
}
