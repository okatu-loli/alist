package migration

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Frozen local shape for the two columns this migration adds (design R4:
// remote-agent reverse connection mode — see internal/model/agent.go and
// internal/bootstrap's server-side reverse supervisor). Mirrors 0017_agent_
// pairing_confirm's TableName(namer) trick: the bare identifier "agent" is
// already taken by 0014 (which created the table), so this migration routes
// its AutoMigrate through the same namer-derived name ("agent" -> "agents",
// TablePrefix-honored) to land on the EXISTING table, adding only the two new
// columns — never a second table.
type agentReverseMode struct {
	ID          uint   `gorm:"primaryKey"`
	Mode        string `gorm:""`
	ReverseAddr string `gorm:""`
}

func (agentReverseMode) TableName(namer schema.Namer) string {
	return namer.TableName("agent")
}

func init() {
	register(&gormigrate.Migration{
		ID: "0018_agent_reverse_mode",
		Migrate: func(tx *gorm.DB) error {
			if err := tx.AutoMigrate(&agentReverseMode{}); err != nil {
				return err
			}
			// Backfill: every pre-existing agent row was paired before reverse
			// mode existed, so it is an OUTBOUND agent. Set mode explicitly to
			// "outbound" (rather than leaving it "" / NULL) so the stored value
			// matches the model's default and the schema-drift/round-trip tests
			// see a concrete mode on legacy rows. IsReverse() treats "" as
			// outbound too, so this is belt-and-suspenders, not load-bearing for
			// safety — but it keeps the data explicit.
			return tx.Model(&agentReverseMode{}).
				Where("mode IS NULL OR mode = ?", "").
				Update("mode", "outbound").Error
		},
		Rollback: func(tx *gorm.DB) error {
			m := tx.Migrator()
			if m.HasColumn(&agentReverseMode{}, "reverse_addr") {
				if err := m.DropColumn(&agentReverseMode{}, "reverse_addr"); err != nil {
					return err
				}
			}
			if m.HasColumn(&agentReverseMode{}, "mode") {
				if err := m.DropColumn(&agentReverseMode{}, "mode"); err != nil {
					return err
				}
			}
			return nil
		},
	})
}
