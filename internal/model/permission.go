package model

// Permission describes a single action that can
// be granted to a role.
type Permission struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	Name        string `json:"name" gorm:"unique"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`
}
