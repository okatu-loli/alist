package migration

import (
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen local shape of the x_agent_tasks table (v4 remote-agent feature,
// design W12, wave W12-3a: server-side foundation for cross-drive copy
// delegation -- task model + capability token + heartbeat dispatch +
// agent-facing capability endpoints, see internal/agent/taskcap.go and
// server/handles/agent_task.go). The bare identifier "agentTask" pluralizes
// through NamingStrategy -- "agentTask" -> "agent_tasks" -- so TablePrefix is
// honored without an explicit Tabler (same trick as 0014_agents). The gorm
// tags mirror the live model (internal/model/agent_task.go) exactly --
// especially the TaskID unique index (what a capability token's task_id is
// looked up by) and the AgentID lookup index (what the heartbeat claim path
// queries by) -- so AutoMigrate lands on identical index names; the
// schema-drift guard enforces they stay in sync.
type agentTask struct {
	ID         uint   `gorm:"primaryKey"`
	TaskID     string `gorm:"uniqueIndex"`
	AgentID    string `gorm:"index"`
	Type       string
	SrcPath    string
	DstDir     string
	CreatorUID uint
	Status     string
	Progress   int
	Detail     string `gorm:"type:text"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func init() {
	register(&gormigrate.Migration{
		ID: "0015_agent_tasks",
		Migrate: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&agentTask{})
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasTable(&agentTask{}) {
				return m.DropTable(&agentTask{})
			}
			return nil
		},
	})
}
