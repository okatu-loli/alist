package migration

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Frozen local shape for the column added by this migration (design W12-4:
// remote-agent reliability -- stale-agent requeue with a capped reassignment
// count, see internal/db/agent_task.go's RequeueStaleAgentTasks). Mirrors
// 0008_permission_display_name's TableName(namer) trick: the bare identifier
// "agentTask" is already taken by 0015 (which created the table), so this
// migration routes its AutoMigrate through the same namer-derived name
// ("agentTask" -> "agent_tasks", TablePrefix-honored) to land on the
// EXISTING table, adding only the new column -- never a second table.
type agentTaskAttempts struct {
	ID       uint `gorm:"primaryKey"`
	Attempts int  `gorm:"default:0"`
}

func (agentTaskAttempts) TableName(namer schema.Namer) string {
	return namer.TableName("agentTask")
}

func init() {
	register(&gormigrate.Migration{
		ID: "0016_agent_task_attempts",
		Migrate: func(tx *gorm.DB) error {
			if err := tx.AutoMigrate(&agentTaskAttempts{}); err != nil {
				return err
			}
			// Backfill: pre-existing rows have no attempts recorded yet. The
			// column default handles fresh inserts; explicitly level any row
			// still holding NULL to 0 (a task that was never requeued).
			return tx.Model(&agentTaskAttempts{}).
				Where("attempts IS NULL").
				Update("attempts", 0).Error
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasColumn(&agentTaskAttempts{}, "attempts") {
				return m.DropColumn(&agentTaskAttempts{}, "attempts")
			}
			return nil
		},
	})
}
