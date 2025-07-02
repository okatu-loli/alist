package model

import (
	"encoding/json"
	"gorm.io/datatypes"
)

// PermissionIdSlice stores permission IDs as a slice.
type PermissionIdSlice []uint

// Role describes a custom role that can be
// assigned to users. Permissions are stored
// as a JSON encoded array of permission IDs.
type Role struct {
	ID             uint              `json:"id" gorm:"primaryKey"`
	Name           string            `json:"name" gorm:"unique"`
	Description    string            `json:"description"`
	Permissions    datatypes.JSON    `json:"permissions"`
	PermissionInfo PermissionIdSlice `json:"permission_info" gorm:"-"`
}

// LoadPermissions parses the Permissions JSON
// field into PermissionInfo.
func (r *Role) LoadPermissions() {
	if len(r.Permissions) == 0 {
		return
	}
	_ = json.Unmarshal(r.Permissions, &r.PermissionInfo)
}

// SavePermissions marshals PermissionInfo into
// the Permissions JSON field.
func (r *Role) SavePermissions() error {
	bs, err := json.Marshal(r.PermissionInfo)
	if err != nil {
		return err
	}
	r.Permissions = bs
	return nil
}
