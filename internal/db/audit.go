package db

import (
	"time"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
)

// CreateAuditLogs inserts a batch in one statement — the audit writer flushes
// buffered entries through here.
func CreateAuditLogs(logs []model.AuditLog) error {
	if len(logs) == 0 {
		return nil
	}
	return errors.WithStack(db.Create(&logs).Error)
}

// AuditLogFilter narrows an audit query; zero values mean "any".
type AuditLogFilter struct {
	ActorUID uint
	// ActionPrefix matches dotted action families ("fs." matches fs.read…).
	ActionPrefix string
	Protocol     string
	Result       string
	From, To     time.Time
}

// GetAuditLogs returns one page (newest first) plus the filtered total.
func GetAuditLogs(f AuditLogFilter, pageIndex, pageSize int) ([]model.AuditLog, int64, error) {
	q := db.Model(&model.AuditLog{})
	if f.ActorUID != 0 {
		q = q.Where("actor_uid = ?", f.ActorUID)
	}
	if f.ActionPrefix != "" {
		q = q.Where("action LIKE ?", f.ActionPrefix+"%")
	}
	if f.Protocol != "" {
		q = q.Where("protocol = ?", f.Protocol)
	}
	if f.Result != "" {
		q = q.Where("result = ?", f.Result)
	}
	if !f.From.IsZero() {
		q = q.Where("ts >= ?", f.From)
	}
	if !f.To.IsZero() {
		q = q.Where("ts <= ?", f.To)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, errors.WithStack(err)
	}
	var logs []model.AuditLog
	if err := q.Order("ts desc, id desc").
		Offset((pageIndex - 1) * pageSize).Limit(pageSize).
		Find(&logs).Error; err != nil {
		return nil, 0, errors.WithStack(err)
	}
	return logs, total, nil
}

// PruneAuditLogs deletes entries older than `before` (retention GC).
func PruneAuditLogs(before time.Time) (int64, error) {
	res := db.Where("ts < ?", before).Delete(&model.AuditLog{})
	return res.RowsAffected, errors.WithStack(res.Error)
}
