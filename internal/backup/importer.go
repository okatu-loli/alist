package backup

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// Restore strategies.
const (
	StrategyMerge     = "merge"     // upsert by natural key; never deletes
	StrategyOverwrite = "overwrite" // replace each domain's contents wholesale
)

// RestoreOptions parameterizes a restore. Name is the archive file name inside
// Dir() (already traversal-checked by the caller). DryRun computes the diff
// without writing.
type RestoreOptions struct {
	Name     string
	Password string
	Strategy string
	DryRun   bool
}

// DomainDiff lists the natural keys a restore would create, update, or delete
// within one domain. Bindings use "left/right" name pairs as their keys.
type DomainDiff struct {
	Create []string `json:"create"`
	Update []string `json:"update"`
	Delete []string `json:"delete"`
}

// RestoreDiff is the shared shape returned by both dry-run and real restores:
// a real restore returns exactly the diff it applied.
type RestoreDiff struct {
	Strategy string                `json:"strategy"`
	DryRun   bool                  `json:"dry_run"`
	Domains  map[string]DomainDiff `json:"domains"`
}

// ValidateName rejects archive names that could escape Dir() via path
// traversal. Backups are always flat base names.
func ValidateName(name string) error {
	if name == "" || name != filepath.Base(name) ||
		strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return errors.Errorf("backup: invalid archive name %q", name)
	}
	return nil
}

// Read decodes and validates an archive from r, decrypting with password when
// it is encrypted. It hard-rejects a format or schema mismatch: a backup is
// never partially restored into a differently-shaped database.
func Read(r io.Reader, password string) (*Archive, error) {
	files, err := readTarGz(r)
	if err != nil {
		return nil, err
	}
	mb, ok := files["manifest.json"]
	if !ok {
		return nil, errors.New("backup: manifest.json missing from archive")
	}
	var manifest Manifest
	if err := json.Unmarshal(mb, &manifest); err != nil {
		return nil, errors.Wrap(err, "backup: parsing manifest")
	}
	if manifest.FormatVersion != FormatVersion {
		return nil, errors.Errorf("backup: unsupported format_version %d (this build expects %d)",
			manifest.FormatVersion, FormatVersion)
	}
	if want := migration.LatestID(); manifest.SchemaVersion != want {
		return nil, errors.Errorf("backup: schema_version %q does not match this database (%q); "+
			"migrate the database to the archive's schema, or restore into a matching build",
			manifest.SchemaVersion, want)
	}

	var key []byte
	if manifest.Encrypted {
		if password == "" {
			return nil, errors.New("backup: archive is encrypted but no password was provided")
		}
		if manifest.KDF == nil {
			return nil, errors.New("backup: encrypted archive is missing kdf params")
		}
		if key, err = deriveKey(password, *manifest.KDF); err != nil {
			return nil, err
		}
	}

	arc := &Archive{Manifest: manifest}
	get := func(domain string) ([]byte, error) {
		raw, ok := files[domain+".json"]
		if !ok {
			return nil, errors.Errorf("backup: %s.json missing from archive", domain)
		}
		if key != nil {
			return decrypt(key, raw)
		}
		return raw, nil
	}
	type target struct {
		domain string
		into   any
	}
	for _, t := range []target{
		{DomainSettings, &arc.Settings},
		{DomainStorages, &arc.Storages},
		{DomainMetas, &arc.Metas},
		{DomainRoles, &arc.Roles},
		{DomainPermissions, &arc.Permissions},
		{DomainUsers, &arc.Users},
		{DomainUserRoles, &arc.UserRoles},
		{DomainRolePermissions, &arc.RolePermissions},
	} {
		data, err := get(t.domain)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, t.into); err != nil {
			return nil, errors.Wrapf(err, "backup: parsing %s", t.domain)
		}
	}

	if manifest.HasSecretsKey {
		if !manifest.Encrypted || key == nil {
			// Export never produces this combination (Options.validate()
			// rejects IncludeSecretsKey without a Password) — a manifest
			// claiming otherwise is corrupt or hand-edited, not a case to
			// silently tolerate.
			return nil, errors.New("backup: manifest declares has_secrets_key on an unencrypted archive, which should never happen")
		}
		raw, ok := files[secretsKeyTarName]
		if !ok {
			return nil, errors.Errorf("backup: manifest declares has_secrets_key but %s is missing from archive", secretsKeyTarName)
		}
		sk, err := decrypt(key, raw)
		if err != nil {
			return nil, errors.Wrap(err, "backup: decrypting secrets key")
		}
		arc.SecretsKey = sk
	}
	return arc, nil
}

// Restore reads Dir()/opts.Name and applies it. Splitting Read from Apply lets
// the CLI restore an archive from any path while the API restores by name from
// the backup dir; both share the same apply engine.
func Restore(opts RestoreOptions) (*RestoreDiff, error) {
	if opts.Strategy != StrategyMerge && opts.Strategy != StrategyOverwrite {
		return nil, errors.Errorf("backup: unknown strategy %q", opts.Strategy)
	}
	if err := ValidateName(opts.Name); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(Dir(), opts.Name))
	if err != nil {
		return nil, errors.Wrap(err, "backup: opening archive")
	}
	arc, err := Read(f, opts.Password)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	return Apply(arc, opts.Strategy, opts.DryRun)
}

// Apply restores an already-decoded archive. A non-dry-run restore first writes
// a full-fidelity snapshot of the current state (pre-restore-<ts>.tar.gz) via
// the exporter, then applies every domain inside one transaction — any failure
// rolls the whole restore back, and the snapshot remains as a manual recovery
// point. Dry-run computes and returns the same diff a real run would apply,
// without writing anything.
func Apply(arc *Archive, strategy string, dryRun bool) (*RestoreDiff, error) {
	if strategy != StrategyMerge && strategy != StrategyOverwrite {
		return nil, errors.Errorf("backup: unknown strategy %q", strategy)
	}
	if dryRun {
		state, err := loadDBState(db.GetDb())
		if err != nil {
			return nil, err
		}
		diff := computeDiff(state, arc, strategy)
		diff.DryRun = true
		return &diff, nil
	}

	// Safety net: snapshot current state before mutating it.
	ts := time.Now().Format("20060102-150405")
	snap := "pre-restore-" + ts + ".tar.gz"
	if _, _, err := exportAs(snap, Options{}); err != nil {
		return nil, errors.Wrap(err, "backup: writing pre-restore snapshot")
	}

	var diff RestoreDiff
	err := db.GetDb().Transaction(func(tx *gorm.DB) error {
		state, err := loadDBState(tx)
		if err != nil {
			return err
		}
		diff = computeDiff(state, arc, strategy)
		return applyToTx(tx, arc, strategy, state)
	})
	if err != nil {
		return nil, err
	}

	// Write the carried secrets key back AFTER the DB commit succeeds, never
	// before: the just-restored Storages/plugin state's enc:v1: values were
	// encrypted under this exact key (it was exported from the same source
	// instance in the same archive), so overwriting the target's key file
	// first — while the transaction could still fail and roll back — would
	// strand the target's PRE-restore data (still encrypted under its OLD
	// key) undecryptable. G4 §5 (fail-loud): a write failure here still
	// surfaces as a restore error, even though the DB half already committed
	// — silently reporting success while the key never landed would be
	// worse, since every credential would then look "restored" but decrypt
	// to garbage.
	if arc.SecretsKey != nil {
		// I2: preserve the CURRENT key (pairs with the pre-restore-<ts>.tar.gz
		// snapshot above, same ts) before overwriting it — otherwise that
		// snapshot's own enc:v1: values become permanently undecryptable the
		// moment the live key file is replaced below, breaking its "complete
		// rollback point" promise. A failure here aborts the restore before
		// the key is touched (fail-loud, not a best-effort skip): losing the
		// snapshot's decryptability silently would defeat the whole point of
		// taking it.
		if err := preserveSecretsKeyForSnapshot(ts); err != nil {
			return nil, err
		}
		if err := restoreSecretsKey(arc.SecretsKey); err != nil {
			return nil, err
		}
	}
	return &diff, nil
}

// dbState is a read-only snapshot of the current natural keys (and their ids,
// for in-place merge updates) used to compute the diff and drive merge upserts.
type dbState struct {
	settingKeys      map[string]struct{}
	userIDByName     map[string]uint
	roleIDByName     map[string]uint
	permIDByName     map[string]uint
	metaIDByPath     map[string]uint
	storageIDByMount map[string]uint
	userRolePairs    map[string]struct{}
	rolePermPairs    map[string]struct{}
}

func loadDBState(tx *gorm.DB) (*dbState, error) {
	s := &dbState{
		settingKeys:      map[string]struct{}{},
		userIDByName:     map[string]uint{},
		roleIDByName:     map[string]uint{},
		permIDByName:     map[string]uint{},
		metaIDByPath:     map[string]uint{},
		storageIDByMount: map[string]uint{},
		userRolePairs:    map[string]struct{}{},
		rolePermPairs:    map[string]struct{}{},
	}
	var settings []model.SettingItem
	if err := tx.Find(&settings).Error; err != nil {
		return nil, errors.Wrap(err, "backup: loading setting keys")
	}
	for _, x := range settings {
		s.settingKeys[x.Key] = struct{}{}
	}
	var users []model.User
	if err := tx.Find(&users).Error; err != nil {
		return nil, errors.Wrap(err, "backup: loading users")
	}
	userNameByID := map[uint]string{}
	for _, u := range users {
		s.userIDByName[u.Username] = u.ID
		userNameByID[u.ID] = u.Username
	}
	var roles []model.Role
	if err := tx.Find(&roles).Error; err != nil {
		return nil, errors.Wrap(err, "backup: loading roles")
	}
	roleNameByID := map[uint]string{}
	for _, r := range roles {
		s.roleIDByName[r.Name] = r.ID
		roleNameByID[r.ID] = r.Name
	}
	var perms []model.Permission
	if err := tx.Find(&perms).Error; err != nil {
		return nil, errors.Wrap(err, "backup: loading permissions")
	}
	permNameByID := map[uint]string{}
	for _, p := range perms {
		s.permIDByName[p.Name] = p.ID
		permNameByID[p.ID] = p.Name
	}
	var metas []model.Meta
	if err := tx.Find(&metas).Error; err != nil {
		return nil, errors.Wrap(err, "backup: loading metas")
	}
	for _, m := range metas {
		s.metaIDByPath[m.Path] = m.ID
	}
	var storages []model.Storage
	if err := tx.Find(&storages).Error; err != nil {
		return nil, errors.Wrap(err, "backup: loading storages")
	}
	for _, st := range storages {
		s.storageIDByMount[st.MountPath] = st.ID
	}
	var urs []model.UserRole
	if err := tx.Find(&urs).Error; err != nil {
		return nil, errors.Wrap(err, "backup: loading user_roles")
	}
	for _, ur := range urs {
		if un, rn := userNameByID[ur.UserID], roleNameByID[ur.RoleID]; un != "" && rn != "" {
			s.userRolePairs[pairKey(un, rn)] = struct{}{}
		}
	}
	var rps []model.RolePermission
	if err := tx.Find(&rps).Error; err != nil {
		return nil, errors.Wrap(err, "backup: loading role_permissions")
	}
	for _, rp := range rps {
		if rn, pn := roleNameByID[rp.RoleID], permNameByID[rp.PermissionID]; rn != "" && pn != "" {
			s.rolePermPairs[pairKey(rn, pn)] = struct{}{}
		}
	}
	return s, nil
}

// pairKey joins a binding's two names into one diff/set key. NUL cannot appear
// in a username/role/permission name, so it is an unambiguous separator.
func pairKey(a, b string) string { return a + "\x00" + b }

// pairDisplay renders a pair key for human-facing diff output.
func pairDisplay(k string) string { return strings.ReplaceAll(k, "\x00", "/") }

func computeDiff(s *dbState, arc *Archive, strategy string) RestoreDiff {
	overwrite := strategy == StrategyOverwrite
	diff := RestoreDiff{Strategy: strategy, DryRun: false, Domains: map[string]DomainDiff{}}

	settingKeys := make([]string, len(arc.Settings))
	for i, x := range arc.Settings {
		settingKeys[i] = x.Key
	}
	diff.Domains[DomainSettings] = diffKeys(settingKeys, setKeys(s.settingKeys), overwrite)

	userKeys := make([]string, len(arc.Users))
	for i, x := range arc.Users {
		userKeys[i] = x.Username
	}
	diff.Domains[DomainUsers] = diffKeys(userKeys, mapKeys(s.userIDByName), overwrite)

	roleKeys := make([]string, len(arc.Roles))
	for i, x := range arc.Roles {
		roleKeys[i] = x.Name
	}
	diff.Domains[DomainRoles] = diffKeys(roleKeys, mapKeys(s.roleIDByName), overwrite)

	permKeys := make([]string, len(arc.Permissions))
	for i, x := range arc.Permissions {
		permKeys[i] = x.Name
	}
	diff.Domains[DomainPermissions] = diffKeys(permKeys, mapKeys(s.permIDByName), overwrite)

	metaKeys := make([]string, len(arc.Metas))
	for i, x := range arc.Metas {
		metaKeys[i] = x.Path
	}
	diff.Domains[DomainMetas] = diffKeys(metaKeys, mapKeys(s.metaIDByPath), overwrite)

	storageKeys := make([]string, len(arc.Storages))
	for i, x := range arc.Storages {
		storageKeys[i] = x.MountPath
	}
	diff.Domains[DomainStorages] = diffKeys(storageKeys, mapKeys(s.storageIDByMount), overwrite)

	urKeys := make([]string, len(arc.UserRoles))
	for i, x := range arc.UserRoles {
		urKeys[i] = pairKey(x.Username, x.RoleName)
	}
	diff.Domains[DomainUserRoles] = displayDiff(diffKeys(urKeys, setKeys(s.userRolePairs), overwrite))

	rpKeys := make([]string, len(arc.RolePermissions))
	for i, x := range arc.RolePermissions {
		rpKeys[i] = pairKey(x.RoleName, x.PermissionName)
	}
	diff.Domains[DomainRolePermissions] = displayDiff(diffKeys(rpKeys, setKeys(s.rolePermPairs), overwrite))

	return diff
}

// diffKeys partitions archive vs db keys. create = archive-only; update =
// present in both (overwrite replaces them, merge upserts them); delete =
// db-only, and only under overwrite. Archive keys are de-duplicated so a
// duplicate natural key does not inflate the counts.
func diffKeys(archiveKeys, dbKeys []string, overwrite bool) DomainDiff {
	dbSet := map[string]struct{}{}
	for _, k := range dbKeys {
		dbSet[k] = struct{}{}
	}
	arcSet := map[string]struct{}{}
	var d DomainDiff
	for _, k := range archiveKeys {
		if _, dup := arcSet[k]; dup {
			continue
		}
		arcSet[k] = struct{}{}
		if _, ok := dbSet[k]; ok {
			d.Update = append(d.Update, k)
		} else {
			d.Create = append(d.Create, k)
		}
	}
	if overwrite {
		for _, k := range dbKeys {
			if _, ok := arcSet[k]; !ok {
				d.Delete = append(d.Delete, k)
			}
		}
	}
	sort.Strings(d.Create)
	sort.Strings(d.Update)
	sort.Strings(d.Delete)
	return d
}

func displayDiff(d DomainDiff) DomainDiff {
	for i := range d.Create {
		d.Create[i] = pairDisplay(d.Create[i])
	}
	for i := range d.Update {
		d.Update[i] = pairDisplay(d.Update[i])
	}
	for i := range d.Delete {
		d.Delete[i] = pairDisplay(d.Delete[i])
	}
	return d
}

func setKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mapKeys(m map[string]uint) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
