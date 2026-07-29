package migration

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Frozen local shape for the two columns this migration adds (design
// W12-2b: SAS visual MITM-confirmation for agent pairing — see
// internal/agent/sas.go and server/handles/agent_pair.go). Mirrors 0016_
// agent_task_attempts's TableName(namer) trick: the bare identifier "agent"
// is already taken by 0014 (which created the table), so this migration
// routes its AutoMigrate through the same namer-derived name ("agent" ->
// "agents", TablePrefix-honored) to land on the EXISTING table, adding only
// the two new columns — never a second table.
type agentPairingConfirm struct {
	ID         uint   `gorm:"primaryKey"`
	Confirmed  bool   `gorm:"default:false"`
	PendingSAS string `gorm:"type:text"`
}

func (agentPairingConfirm) TableName(namer schema.Namer) string {
	return namer.TableName("agent")
}

func init() {
	register(&gormigrate.Migration{
		ID: "0017_agent_pairing_confirm",
		Migrate: func(tx *gorm.DB) error {
			if err := tx.AutoMigrate(&agentPairingConfirm{}); err != nil {
				return err
			}
			// Backfill: every pre-existing agent row was paired before this
			// wave existed, i.e. under the old "created Enabled=true,
			// no confirmation step" flow — treat all of them as already
			// confirmed rather than retroactively locking out agents an
			// admin already trusted and is actively relying on. Only agents
			// paired AFTER this migration (via the new PairAgentComplete)
			// ever start life with Confirmed=false.
			return tx.Model(&agentPairingConfirm{}).
				Where("confirmed IS NULL OR confirmed = ?", false).
				Update("confirmed", true).Error
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasColumn(&agentPairingConfirm{}, "pending_sas") {
				if err := m.DropColumn(&agentPairingConfirm{}, "pending_sas"); err != nil {
					return err
				}
			}
			if m.HasColumn(&agentPairingConfirm{}, "confirmed") {
				if err := m.DropColumn(&agentPairingConfirm{}, "confirmed"); err != nil {
					return err
				}
			}
			return nil
		},
	})
}
