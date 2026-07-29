package db

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"slices"
)

func GetUserByRole(role int) (*model.User, error) {
	var users []model.User
	if err := db.Find(&users).Error; err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].Role.Contains(role) {
			return &users[i], nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func GetUsersByRole(roleID int) ([]model.User, error) {
	var users []model.User
	if err := db.Find(&users).Error; err != nil {
		return nil, err
	}
	var result []model.User
	for _, u := range users {
		if slices.Contains(u.Role, roleID) {
			result = append(result, u)
		}
	}
	return result, nil
}

func GetUserByName(username string) (*model.User, error) {
	user := model.User{Username: username}
	if err := db.Where(user).First(&user).Error; err != nil {
		return nil, errors.Wrapf(err, "failed find user")
	}
	return &user, nil
}

func GetUserBySSOID(ssoID string) (*model.User, error) {
	user := model.User{SsoID: ssoID}
	if err := db.Where(user).First(&user).Error; err != nil {
		return nil, errors.Wrapf(err, "The single sign on platform is not bound to any users")
	}
	return &user, nil
}

func GetUserById(id uint) (*model.User, error) {
	var u model.User
	if err := db.First(&u, id).Error; err != nil {
		return nil, errors.Wrapf(err, "failed get old user")
	}
	return &u, nil
}

func CreateUser(u *model.User) error {
	return errors.WithStack(db.Create(u).Error)
}

func UpdateUser(u *model.User) error {
	return errors.WithStack(db.Save(u).Error)
}

func GetUsers(pageIndex, pageSize int) (users []model.User, count int64, err error) {
	userDB := db.Model(&model.User{})
	if err := userDB.Count(&count).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get users count")
	}
	if err := userDB.Order(columnName("id")).Offset((pageIndex - 1) * pageSize).Limit(pageSize).Find(&users).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get find users")
	}
	return users, count, nil
}

func GetAllUsers() ([]model.User, error) {
	var users []model.User
	if err := db.Find(&users).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return users, nil
}

func DeleteUserById(id uint) error {
	return errors.WithStack(db.Delete(&model.User{}, id).Error)
}

func UpdateAuthn(userID uint, authn string) error {
	return db.Model(&model.User{ID: userID}).Update("authn", authn).Error
}

// CountEnabledUsersWithRoleExcluding counts users that are enabled (not
// disabled), hold roleID in their role binding, and are not excludeUserID.
// The role column is a JSON array with no portable containment operator, so the
// filter is applied in Go (mirroring GetUsersByRole) to stay correct across the
// MySQL/Postgres/SQLite backends rather than depending on JSON_CONTAINS.
func CountEnabledUsersWithRoleExcluding(roleID int, excludeUserID uint) (int64, error) {
	var users []model.User
	if err := db.Find(&users).Error; err != nil {
		return 0, errors.WithStack(err)
	}
	var count int64
	for i := range users {
		if users[i].ID == excludeUserID || users[i].Disabled {
			continue
		}
		if users[i].Role.Contains(roleID) {
			count++
		}
	}
	return count, nil
}
