package model

import (
	"fmt"
	"time"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/pkg/utils/random"
	"github.com/pkg/errors"
)

const (
	GENERAL = iota
	GUEST   // only one exists
	ADMIN
	NEWGENERAL
)

const StaticHashSalt = "https://github.com/alist-org/alist"

type User struct {
	ID          uint   `json:"id" gorm:"primaryKey"`                      // unique key
	Username    string `json:"username" gorm:"unique" binding:"required"` // username
	PwdHash     string `json:"-"`                                         // password hash
	PwdTS       int64  `json:"-"`                                         // password timestamp
	Salt        string `json:"-"`                                         // unique salt
	Password    string `json:"password"`                                  // password
	Role        Roles  `json:"role" gorm:"type:text"`                     // user's roles
	RolesDetail []Role `json:"-" gorm:"-"`
	Disabled    bool   `json:"disabled"`
	// Permission is the legacy 17-bit mask. The column is kept for the
	// baseline-frozen schema (same treatment as Authn); all runtime reads
	// were retired in v4 Step 5 — authorization is RBAC5 (op.Authorize).
	Permission int32  `json:"permission"`
	OtpSecret  string `json:"-"`
	SsoID      string `json:"sso_id"` // unique by sso platform
	// Authn holds serialized WebAuthn credentials. The column is kept for the
	// future WebAuthn plugin (baseline-frozen schema); runtime code was removed
	// with the v4 core strip.
	Authn string `gorm:"type:text" json:"-"`
	// Language is the user's explicit locale preference (e.g. "zh-CN"). Empty
	// means "follow the request's Accept-Language". Added in migration 0006.
	Language string `json:"language" gorm:"default:''"`

	// Scoped marks a synthetic principal minted from an extension scoped token
	// (Step 10): its authorization policy is exactly ScopedPermissions, never
	// role-derived. Both fields are request-scoped and never persisted.
	Scoped bool `json:"-" gorm:"-"`
	// ScopedPermissions is the complete effective policy of a Scoped principal:
	// op.UserPermissions returns it verbatim, so op.Authorize confines the
	// token to exactly the declared storage paths (read-only unless a scope
	// granted write).
	ScopedPermissions []Permission `json:"-" gorm:"-"`
}

func (u *User) IsGuest() bool {
	return u.Role.Contains(GUEST)
}

func (u *User) IsAdmin() bool {
	return u.Role.Contains(ADMIN)
}

func (u *User) ValidateRawPassword(password string) error {
	return u.ValidatePwdStaticHash(StaticHash(password))
}

func (u *User) ValidatePwdStaticHash(pwdStaticHash string) error {
	if pwdStaticHash == "" {
		return errors.WithStack(errs.EmptyPassword)
	}
	if u.PwdHash != HashPwd(pwdStaticHash, u.Salt) {
		return errors.WithStack(errs.WrongPassword)
	}
	return nil
}

func (u *User) SetPassword(pwd string) *User {
	u.Salt = random.String(16)
	u.PwdHash = TwoHashPwd(pwd, u.Salt)
	u.PwdTS = time.Now().Unix()
	return u
}

func (u *User) JoinPath(reqPath string) (string, error) {
	return utils.FixAndCleanPath(reqPath), nil
}

func StaticHash(password string) string {
	return utils.HashData(utils.SHA256, []byte(fmt.Sprintf("%s-%s", password, StaticHashSalt)))
}

func HashPwd(static string, salt string) string {
	return utils.HashData(utils.SHA256, []byte(fmt.Sprintf("%s-%s", static, salt)))
}

func TwoHashPwd(password string, salt string) string {
	return HashPwd(StaticHash(password), salt)
}
