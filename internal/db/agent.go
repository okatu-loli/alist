package db

import (
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// Remote-agent CRUD (design W12-1: data-layer + admin-endpoint foundation
// only). No caching layer, same reasoning as app_password.go — an agent row
// gates real storage access once channel/task machinery lands (W12-2/3), so
// every read must hit the DB rather than risk serving a stale Enabled/
// whitelist value from a cache.

// CreateAgent inserts a new agent row.
func CreateAgent(a *model.Agent) error {
	return errors.WithStack(db.Create(a).Error)
}

// GetAgent loads an agent by primary key. A missing id maps to
// errs.AgentNotFound.
func GetAgent(id uint) (*model.Agent, error) {
	var a model.Agent
	err := db.First(&a, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errs.AgentNotFound
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &a, nil
}

// GetAgentByAgentID loads an agent by its self-reported AgentID (the pubkey
// fingerprint, see agent.FingerprintPubKey) — the lookup key the pairing-
// complete and heartbeat handlers use, since neither ever sees the row's
// numeric primary key. A missing id maps to errs.AgentNotFound, same as
// GetAgent.
func GetAgentByAgentID(agentID string) (*model.Agent, error) {
	var a model.Agent
	err := db.Where("agent_id = ?", agentID).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errs.AgentNotFound
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &a, nil
}

// ListAgents returns every paired agent, id-ordered.
func ListAgents() ([]model.Agent, error) {
	var as []model.Agent
	if err := db.Order("id").Find(&as).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return as, nil
}

// UpdateAgent persists the full row (caller already merged the partial
// update onto a freshly loaded row — same pattern as op.UpdateUser).
func UpdateAgent(a *model.Agent) error {
	return errors.WithStack(db.Save(a).Error)
}

// DeleteAgent removes an agent row by id.
func DeleteAgent(id uint) error {
	return errors.WithStack(db.Delete(&model.Agent{}, id).Error)
}
