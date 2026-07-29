// Package backup is the backend backup/restore engine (rollout Step 13b).
//
// A backup is a tar.gz holding a plaintext manifest.json plus one <domain>.json
// per exported domain. It is a data-safety feature: v3 did this in the browser
// (the admin page fetched every settings/users/storages/metas row over the API
// and stitched a JSON, then wrote it back row by row on restore) — slow, easy to
// interrupt, version-blind, and gone the moment you leave the browser. Here it
// is a first-class backend capability with a schema fingerprint, atomic restore,
// and merge/overwrite/dry-run semantics; the frontend only triggers it.
//
// What is and is NOT in a backup:
//   - Exported: settings, users, roles, permissions, metas, storages, and the
//     two RBAC bindings (user_roles, role_permissions). Bindings travel as NAME
//     pairs, not ids, so they survive the id shifts that happen when rows are
//     re-inserted into a different instance (ids are re-resolved at import).
//   - Excluded: sessions and tokens are runtime state, not configuration.
//     Signing keys are excluded too — a restored instance generates its own and
//     re-signs; carrying another instance's signing secrets would be both a
//     leak and pointless. This is recorded on Manifest below because it is a
//     deliberate scope decision, not an omission.
package backup

import (
	"time"

	"github.com/alist-org/alist/v3/internal/model"
)

// FormatVersion is the on-disk envelope version. Bumped only when the archive
// *structure* changes (not when the DB schema changes — that is SchemaVersion).
const FormatVersion = 1

// Domain names double as the <name>.json member inside the archive and the key
// under Manifest.Domains. importDomainOrder below fixes apply ordering.
const (
	DomainSettings        = "settings"
	DomainUsers           = "users"
	DomainRoles           = "roles"
	DomainPermissions     = "permissions"
	DomainMetas           = "metas"
	DomainStorages        = "storages"
	DomainUserRoles       = "user_roles"
	DomainRolePermissions = "role_permissions"
)

// Manifest is the archive's self-description. It stays plaintext even when the
// domain payloads are encrypted, so a backup can be identified and validated
// without the password.
//
// SchemaVersion is migration.LatestID() at export time: restore rejects any
// archive whose SchemaVersion != the running binary's LatestID(), so a backup
// is never partially applied into a differently-shaped database. KDF is present
// exactly when Encrypted is true.
type Manifest struct {
	FormatVersion int            `json:"format_version"`
	SchemaVersion string         `json:"schema_version"`
	CreatedAt     time.Time      `json:"created_at"`
	Domains       map[string]int `json:"domains"` // domain name -> row count
	Encrypted     bool           `json:"encrypted"`
	Sanitized     bool           `json:"sanitized,omitempty"`
	KDF           *KDFParams     `json:"kdf,omitempty"`
	// HasSecretsKey records whether the archive's secrets.key member carries
	// the shared secrets key (G4 §4) — never the key itself, which stays
	// inside the encrypted member. Recording only a bool in the always-
	// plaintext manifest is safe: it tells restore whether to expect the
	// member without revealing anything about the key.
	HasSecretsKey bool `json:"has_secrets_key,omitempty"`
}

// KDFParams records how the AES-256 key was derived from the password so that
// restore can reproduce it. Only the salt and cost live here; the password
// never touches the archive.
type KDFParams struct {
	Algo       string `json:"algo"`       // "pbkdf2-sha256"
	Salt       string `json:"salt"`       // base64 std encoding
	Iterations int    `json:"iterations"` // pbkdf2 rounds
	KeyLen     int    `json:"key_len"`    // derived key length in bytes (32 = AES-256)
}

// Archive is the fully-decoded, in-memory form of a backup: what the exporter
// produces from the database and what the importer decodes before applying.
//
// The five entity domains reuse the live model structs directly (every real
// column carries a json tag, so a plain json round-trip is loss-free) — except
// users, whose password hash / salt / OTP secret are json:"-" on model.User and
// would silently drop; userRecord below captures the full row. The tie between
// this format and the model json tags is guarded by SchemaVersion: any column
// change goes through a migration, which changes LatestID() and invalidates
// older archives.
type Archive struct {
	Manifest        Manifest
	Settings        []model.SettingItem
	Users           []userRecord
	Roles           []model.Role
	Permissions     []model.Permission
	Metas           []model.Meta
	Storages        []model.Storage
	UserRoles       []userRoleBinding
	RolePermissions []rolePermissionBinding
	// SecretsKey is the shared secrets key (G4 §4), present only when
	// Manifest.HasSecretsKey is true. It travels in its own encrypted tar
	// member (secretsKeyTarName), never as a "domain" — it is key material,
	// not a DB row set — and is never marshaled: nothing here ever calls
	// json.Marshal(Archive) or json.Marshal(arc) as a whole (each domain is
	// serialized individually via mustJSON), so no json tag is needed to
	// keep it out of a payload.
	SecretsKey []byte
}

// userRecord is the full-fidelity serialization of model.User: it re-adds the
// credential columns that model.User marks json:"-" (they must survive a backup
// or restored users could not log in) and drops the transient RolesDetail
// (gorm:"-"). Field names mirror the DB columns.
type userRecord struct {
	ID         uint        `json:"id"`
	Username   string      `json:"username"`
	PwdHash    string      `json:"pwd_hash"`
	PwdTS      int64       `json:"pwd_ts"`
	Salt       string      `json:"salt"`
	Password   string      `json:"password"`
	Role       model.Roles `json:"role"`
	Disabled   bool        `json:"disabled"`
	Permission int32       `json:"permission"`
	OtpSecret  string      `json:"otp_secret"`
	SsoID      string      `json:"sso_id"`
	Authn      string      `json:"authn"`
	Language   string      `json:"language"`
}

func userToRecord(u model.User) userRecord {
	return userRecord{
		ID:         u.ID,
		Username:   u.Username,
		PwdHash:    u.PwdHash,
		PwdTS:      u.PwdTS,
		Salt:       u.Salt,
		Password:   u.Password,
		Role:       u.Role,
		Disabled:   u.Disabled,
		Permission: u.Permission,
		OtpSecret:  u.OtpSecret,
		SsoID:      u.SsoID,
		Authn:      u.Authn,
		Language:   u.Language,
	}
}

func (r userRecord) toModel() model.User {
	u := model.User{
		Username:   r.Username,
		PwdHash:    r.PwdHash,
		PwdTS:      r.PwdTS,
		Salt:       r.Salt,
		Password:   r.Password,
		Role:       r.Role,
		Disabled:   r.Disabled,
		Permission: r.Permission,
		OtpSecret:  r.OtpSecret,
		SsoID:      r.SsoID,
		Authn:      r.Authn,
		Language:   r.Language,
	}
	u.ID = r.ID
	return u
}

// userRoleBinding and rolePermissionBinding are the name-keyed forms of
// model.UserRole / model.RolePermission. Names, not ids, so a binding still
// points at the right rows after they are re-inserted with different ids.
type userRoleBinding struct {
	Username string `json:"username"`
	RoleName string `json:"role_name"`
}

type rolePermissionBinding struct {
	RoleName       string `json:"role_name"`
	PermissionName string `json:"permission_name"`
}

// importDomainOrder is the order restore applies domains so that a binding is
// only resolved after both of the rows it names already exist: entities first,
// then the bindings that reference them.
var importDomainOrder = []string{
	DomainSettings,
	DomainStorages,
	DomainMetas,
	DomainRoles,
	DomainPermissions,
	DomainUsers,
	DomainUserRoles,
	DomainRolePermissions,
}
