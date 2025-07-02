package v3_42_0

import (
	"encoding/json"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// ConvertUserRole converts old int role to JSON array format
func ConvertUserRole() {
	d := db.GetDb()
	var users []model.User
	if err := d.Find(&users).Error; err != nil {
		utils.Log.Errorf("failed to load users: %v", err)
		return
	}
	for i := range users {
		var arr []int
		if err := json.Unmarshal(users[i].Role, &arr); err != nil || len(arr) == 0 {
			var single int
			if err2 := json.Unmarshal(users[i].Role, &single); err2 == nil {
				arr = []int{single}
			} else {
				continue
			}
		}
		users[i].RoleInfo = arr
		if err := users[i].SaveRoles(); err != nil {
			utils.Log.Errorf("failed marshal roles for user %d: %v", users[i].ID, err)
			continue
		}
		if err := d.Model(&model.User{ID: users[i].ID}).Update("role", users[i].Role).Error; err != nil {
			utils.Log.Errorf("failed update user %d: %v", users[i].ID, err)
		}
	}
}
