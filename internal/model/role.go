package model

import "gorm.io/datatypes"

// Role describes a custom role that can be
// assigned to users. Permissions are stored
// as a JSON encoded array of permission IDs.
type Role struct {
	ID          uint           `json:"id" gorm:"primaryKey"`
	Name        string         `json:"name" gorm:"unique"`
	Description string         `json:"description"`
	Permissions datatypes.JSON `json:"permissions"`
}
