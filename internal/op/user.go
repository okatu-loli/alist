package op

import (
	"time"

	"github.com/Xhofe/go-cache"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/singleflight"
)

var userCache = cache.NewMemCache(cache.WithShards[*model.User](2))
var userG singleflight.Group[*model.User]
var guestUser *model.User
var adminUser *model.User

func GetAdmin() (*model.User, error) {
	if adminUser == nil {
		role, err := GetRoleByName("admin")
		if err != nil {
			return nil, err
		}
		user, err := db.GetUserByRole(int(role.ID))
		if err != nil {
			return nil, err
		}
		adminUser = user
	}
	return adminUser, nil
}

func GetGuest() (*model.User, error) {
	if guestUser == nil {
		role, err := GetRoleByName("guest")
		if err != nil {
			return nil, err
		}
		user, err := db.GetUserByRole(int(role.ID))
		if err != nil {
			return nil, err
		}
		guestUser = user
	}
	return guestUser, nil
}

func GetUserByRole(role int) (*model.User, error) {
	return db.GetUserByRole(role)
}

func GetUsersByRole(role int) ([]model.User, error) {
	return db.GetUsersByRole(role)
}

func GetUserByName(username string) (*model.User, error) {
	if username == "" {
		return nil, errs.EmptyUsername
	}
	if user, ok := userCache.Get(username); ok {
		return user, nil
	}
	user, err, _ := userG.Do(username, func() (*model.User, error) {
		_user, err := db.GetUserByName(username)
		if err != nil {
			return nil, err
		}
		userCache.Set(username, _user, cache.WithEx[*model.User](time.Hour))
		return _user, nil
	})
	return user, err
}

func GetUserById(id uint) (*model.User, error) {
	return db.GetUserById(id)
}

// GetUserBySSOID resolves the user whose SsoID equals ssoID (the federated link
// key "<provider_key>:<subject>"). It is the sub-based link lookup the SSO
// identity resolver consults first: a stable, non-forgeable match. An empty
// ssoID is refused rather than matching the many users with no link (a blank
// SsoID column), which would otherwise return an arbitrary account.
func GetUserBySSOID(ssoID string) (*model.User, error) {
	if ssoID == "" {
		return nil, errs.EmptySSOID
	}
	return db.GetUserBySSOID(ssoID)
}

func GetUsers(pageIndex, pageSize int) (users []model.User, count int64, err error) {
	return db.GetUsers(pageIndex, pageSize)
}

func CreateUser(u *model.User) error {
	err := db.CreateUser(u)
	if err != nil {
		return err
	}
	if err := syncUserRoles(u); err != nil {
		return err
	}
	return nil
}

func DeleteUserById(id uint) error {
	old, err := db.GetUserById(id)
	if err != nil {
		return err
	}
	// The guest user and the built-in "admin" account are structurally
	// undeletable. Other admin users are deletable, but never the last enabled
	// one — that would lock out the whole IsAdmin-gated admin surface (spec §7).
	if old.IsGuest() || old.Username == "admin" {
		return errs.DeleteAdminOrGuest
	}
	if old.IsAdmin() && !old.Disabled {
		count, err := CountEnabledAdminsExcluding(id)
		if err != nil {
			return err
		}
		if count == 0 {
			return errs.LastAdmin
		}
	}
	userCache.Del(old.Username)
	if err := db.DeleteUserById(id); err != nil {
		return err
	}
	_ = db.DeleteUserRolesByUser(id)
	return nil
}

func UpdateUser(u *model.User) error {
	old, err := db.GetUserById(u.ID)
	if err != nil {
		return err
	}
	// Last-admin lockout backstop (spec §7), mirroring the handler guard in
	// server/handles/user.go and the DeleteUserById check above: if the stored
	// user is currently an enabled admin and this update would strip that
	// status — disabling it or removing the ADMIN role — refuse when it is the
	// last enabled admin left. Keeps op-layer callers (not just the handler)
	// from locking out the IsAdmin-gated admin surface.
	if old.IsAdmin() && !old.Disabled && (u.Disabled || !u.IsAdmin()) {
		count, err := CountEnabledAdminsExcluding(old.ID)
		if err != nil {
			return err
		}
		if count == 0 {
			return errs.LastAdmin
		}
	}
	if u.IsAdmin() {
		adminUser = nil
	}
	if u.IsGuest() {
		guestUser = nil
	}
	userCache.Del(old.Username)
	if err := db.UpdateUser(u); err != nil {
		return err
	}
	if err := syncUserRoles(u); err != nil {
		return err
	}
	return nil
}

func Cancel2FAByUser(u *model.User) error {
	u.OtpSecret = ""
	return UpdateUser(u)
}

func Cancel2FAById(id uint) error {
	user, err := db.GetUserById(id)
	if err != nil {
		return err
	}
	return Cancel2FAByUser(user)
}

func DelUserCache(username string) error {
	user, err := GetUserByName(username)
	if err != nil {
		return err
	}
	if user.IsAdmin() {
		adminUser = nil
	}
	if user.IsGuest() {
		guestUser = nil
	}
	userCache.Del(username)
	return nil
}

// CountEnabledAdminsExcluding counts enabled admin users other than userID.
// An admin is any user whose role binding contains model.ADMIN (the identifier
// IsAdmin matches on), so this stays correct whether or not the built-in
// "admin" role row is present — the last-admin lockout invariant depends on it.
func CountEnabledAdminsExcluding(userID uint) (int64, error) {
	return db.CountEnabledUsersWithRoleExcluding(model.ADMIN, userID)
}
