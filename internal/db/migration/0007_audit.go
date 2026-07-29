package migration

import (
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Frozen snapshot of the audit_logs table as created by this migration
// (rollout Step 12). The struct name pluralizes to the same "audit_logs" the
// live model.AuditLog maps to; TablePrefix still applies via NamingStrategy.
type auditLog struct {
	ID        uint      `gorm:"primaryKey"`
	ActorUID  uint      `gorm:"index"`
	Actor     string    `gorm:"size:256"`
	Action    string    `gorm:"size:64;index"`
	TargetRef string    `gorm:"size:1024"`
	Protocol  string    `gorm:"size:16"`
	IP        string    `gorm:"size:64"`
	Result    string    `gorm:"size:16;index"`
	Ts        time.Time `gorm:"index"`
}

func init() {
	register(&gormigrate.Migration{
		ID: "0007_audit",
		Migrate: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&auditLog{})
		},
		Rollback: func(tx *gorm.DB) error {
			return tx.Migrator().DropTable(&auditLog{})
		},
	})
}
