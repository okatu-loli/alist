// Package migratev4 implements the `alist migrate v4` upgrade orchestrator
// (rollout Step 14): a one-shot, read-only-source, write-to-a-fresh-target
// migration of a v3 database into a v4 data directory.
//
// It never writes the source. The target is a brand-new v4 data dir; the tool
// refuses to run if the target already holds a database. Rollback is therefore
// just "keep using the old data dir" — see docs/UPGRADE.md.
package migratev4

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/bootstrap/data"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/migratev4/legacy"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// Options configures a migration run.
type Options struct {
	// Source is a v3 data directory (with config.json) or a v3 sqlite .db file.
	Source string
	// SourcePrefix overrides the table prefix (default: from config.json, or
	// "x_" for a bare sqlite file).
	SourcePrefix string
	// Target is the v4 data directory to create and populate. Must not already
	// contain a database.
	Target string
	// DryRun reads and converts everything but writes nothing (and does not
	// create the target). It prints the same report a real run would.
	DryRun bool
}

// Run executes the migration (or dry-run) and returns the outcome report.
func Run(opts Options) (*Report, error) {
	if opts.Source == "" {
		return nil, errors.New("migrate v4: --source is required")
	}
	// data.InitialSettings() (used to compute the valid-key set) reads default
	// task worker counts off conf.Conf. On this code path conf has not been
	// initialised yet (the source is read before the target's InitConfig), so
	// seed sane defaults; initTarget replaces this with the target's config.
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}

	src, err := readSource(opts.Source, opts.SourcePrefix)
	if err != nil {
		return nil, err
	}

	// The conversion plan is computed identically for dry-run and real runs so
	// the preview matches the result.
	plan := buildPlan(src)

	if opts.DryRun {
		return plan.report(true), nil
	}

	if opts.Target == "" {
		return nil, errors.New("migrate v4: --target is required (unless --dry-run)")
	}
	if utils.Exists(filepath.Join(opts.Target, "data.db")) {
		return nil, fmt.Errorf("migrate v4: target %q already contains a database; refusing to overwrite", opts.Target)
	}

	if err := initTarget(opts.Target); err != nil {
		return nil, err
	}
	if err := plan.write(); err != nil {
		return nil, err
	}
	return plan.report(false), nil
}

// plan is the fully-resolved conversion, computed once from the source.
type plan struct {
	src *source

	settingsToCarry []model.SettingItem
	settingsDropped []string

	// storagesDisabled is the set of storage IDs to import disabled (unknown
	// driver), for the report; write() re-derives from the same predicate.
	disabledStorage  map[uint]bool
	storagesDisabled []string

	// basePathScopes lists users whose v3 base_path was converted into a
	// private permission scope role (fence preservation). Computed here so the
	// dry-run preview matches the real run; writeUsers re-derives the same
	// predicate when it creates the roles.
	basePathScopes []BasePathConversion

	validKeys map[string]bool
}

func buildPlan(src *source) *plan {
	p := &plan{src: src, disabledStorage: map[uint]bool{}, validKeys: map[string]bool{}}

	for _, item := range data.InitialSettings() {
		p.validKeys[item.Key] = true
	}
	for _, s := range src.settings {
		if s.Key == conf.VERSION {
			continue // v4 owns its version string
		}
		if p.validKeys[s.Key] {
			p.settingsToCarry = append(p.settingsToCarry, model.SettingItem{
				Key: s.Key, Value: s.Value, Type: s.Type, Group: s.Group, Flag: s.Flag,
			})
		} else {
			p.settingsDropped = append(p.settingsDropped, s.Key)
		}
	}

	for _, st := range src.storages {
		if _, err := op.GetDriver(st.Driver); err != nil {
			p.disabledStorage[st.ID] = true
			p.storagesDisabled = append(p.storagesDisabled, fmt.Sprintf("%s (%s)", st.MountPath, st.Driver))
		}
	}

	// Same predicate writeUsers uses to convert a v3 base_path into a scope role.
	for _, u := range src.users {
		if bp := utils.FixAndCleanPath(u.BasePath); u.BasePath != "" && bp != "/" {
			p.basePathScopes = append(p.basePathScopes, BasePathConversion{Username: u.Username, Path: bp})
		}
	}
	return p
}

func (p *plan) report(dryRun bool) *Report {
	r := &Report{
		DryRun:           dryRun,
		SettingsCarried:  len(p.settingsToCarry),
		SettingsDropped:  p.settingsDropped,
		UsersCarried:     len(p.src.users),
		StoragesCarried:  len(p.src.storages),
		StoragesDisabled: p.storagesDisabled,
		MetasCarried:     len(p.src.metas),
		TasksSkipped:     p.src.taskCount,
		NotCarried:       p.src.dropped,
	}
	// Roles carried is resolved during write(); for the report we count the
	// source roles plus the three builtin roles the target always ends up with.
	r.RolesCarried = rolesCarriedCount(p.src)
	for _, c := range p.basePathScopes {
		r.AddBasePathConversion(c.Username, c.Path)
	}
	return r
}

// rolesCarriedCount reports how many role rows the target will hold: every
// source role, plus whichever of guest/admin/general are not already among them.
func rolesCarriedCount(src *source) int {
	present := map[string]bool{}
	for _, r := range src.roles {
		present[r.Name] = true
	}
	count := len(src.roles)
	for _, name := range []string{"guest", "admin", "general"} {
		if !present[name] {
			count++
		}
	}
	return count
}

// initTarget wires the process configuration to the target directory, opens a
// fresh v4 database there, and runs the full migration chain to create the
// (empty) v4 schema.
func initTarget(target string) error {
	flags.MigrateMode = "off"
	flags.DataDir = target
	bootstrap.InitConfig()
	bootstrap.Log()
	bootstrap.InitDB()
	if err := migration.New(db.GetDb(), conf.Conf.Database.TablePrefix).Up(); err != nil {
		return errors.Wrap(err, "migrate v4: creating target schema")
	}
	return nil
}

// write converts and inserts the source snapshot into the (already migrated)
// target database. Order matters: roles before users (so role ids exist for
// binding), storages/metas/settings independently.
func (p *plan) write() error {
	generalID, err := p.writeRoles()
	if err != nil {
		return err
	}
	if err := p.writeUsers(generalID); err != nil {
		return err
	}
	if err := p.writeStorages(); err != nil {
		return err
	}
	if err := p.writeMetas(); err != nil {
		return err
	}
	return p.writeSettings()
}

// writeRoles carries every source role verbatim (id preserved) and then ensures
// the builtin guest/admin/general roles exist. op.CreateRole dual-writes the
// scope permission rows; anchors are left unresolved (no storages are loaded
// yet) and heal on the first real boot. Returns the general role id used to map
// legacy GENERAL users.
func (p *plan) writeRoles() (uint, error) {
	for _, lr := range p.src.roles {
		role := &model.Role{
			ID:               lr.ID,
			Name:             lr.Name,
			Description:      lr.Description,
			Default:          lr.Default,
			PermissionScopes: parseScopes(lr.RawPermission),
		}
		if err := op.CreateRole(role); err != nil {
			return 0, errors.Wrapf(err, "migrate v4: carrying role %q", lr.Name)
		}
	}

	guest, err := ensureRole("guest", "Guest", uint(model.GUEST), 0)
	if err != nil {
		return 0, err
	}
	admin, err := ensureRole("admin", "Administrator", uint(model.ADMIN), 0xFFFF)
	if err != nil {
		return 0, err
	}
	_ = guest
	_ = admin
	general, err := ensureRole("general", "General User", 0, 0)
	if err != nil {
		return 0, err
	}
	return general.ID, nil
}

// ensureRole returns the role by name, creating it with the given scope if
// missing. pinnedID is used only on creation and only when non-zero (guest/admin
// pin their ids to the v4 constants; general takes an auto id).
func ensureRole(name, description string, pinnedID uint, permission int32) (*model.Role, error) {
	r, err := op.GetRoleByName(name)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.Wrapf(err, "migrate v4: looking up %q role", name)
	}
	r = &model.Role{
		Name:             name,
		Description:      description,
		PermissionScopes: []model.PermissionEntry{{Path: "/", Permission: permission}},
	}
	if pinnedID != 0 {
		r.ID = pinnedID
	}
	if err := op.CreateRole(r); err != nil {
		return nil, errors.Wrapf(err, "migrate v4: creating %q role", name)
	}
	return r, nil
}

// writeUsers carries each v3 user verbatim (password hash/salt/timestamp and
// base path unchanged — no rehash) and rebinds roles. The legacy int role
// values are mapped by the fixed v3 rule, identical to
// internal/bootstrap/patch/v3_46_0/convert_role.go.
func (p *plan) writeUsers(generalID uint) error {
	guest, err := op.GetRoleByName("guest")
	if err != nil {
		return errors.Wrap(err, "migrate v4: guest role")
	}
	admin, err := op.GetRoleByName("admin")
	if err != nil {
		return errors.Wrap(err, "migrate v4: admin role")
	}
	mapRole := func(v int) uint {
		switch v {
		case model.ADMIN:
			return admin.ID
		case model.GUEST:
			return guest.ID
		case model.GENERAL:
			return generalID
		default:
			return uint(v)
		}
	}

	for _, lu := range p.src.users {
		roleIDs := make([]uint, 0)
		for _, v := range parseRoleInts(lu.Role) {
			roleIDs = append(roleIDs, mapRole(v))
		}
		// v3 base_path was an ABSOLUTE confinement: the user could access only
		// that subtree, because every request path was physically prefixed by
		// base_path — NO role could let them escape it. v4 has no BasePath and
		// every role scope is an independent grant, so to preserve the fence we
		// must neutralise EVERY out-of-fence grant, not just the built-in
		// read-all "general" role: any other carried role (e.g. a custom v3 role
		// scoped to "/" ) would otherwise re-open access above the fence and
		// silently downgrade it (W4 red-team #4). We therefore drop ALL of the
		// user's carried roles and grant a single private scope role covering
		// exactly the old base_path with the user's own legacy permission bits
		// (ActRead is implied by scope coverage). The one binding we keep is the
		// built-in admin role: v3 admins bypassed base_path entirely and were
		// never fenced, so a would-be admin stays an admin. This realises the
		// spec's "用户无其他更广 scope" invariant (§3) and makes the out-of-fence
		// Authorize denial hold (§5, red-team headline). The conversion is listed
		// in the report for admin review (v3 confinement vs v4 grant differ).
		if bp := utils.FixAndCleanPath(lu.BasePath); lu.BasePath != "" && bp != "/" {
			scopeRole := &model.Role{
				Name:        "migrated-scope:" + lu.Username,
				Description: "auto-migrated v3 base_path fence",
				PermissionScopes: []model.PermissionEntry{
					{Path: bp, Permission: lu.Permission},
				},
			}
			if err := op.CreateRole(scopeRole); err != nil {
				return errors.Wrapf(err, "migrate v4: base_path scope role for %q", lu.Username)
			}
			roleIDs = keepOnlyAdmin(roleIDs, admin.ID)
			roleIDs = append(roleIDs, scopeRole.ID)
		}
		roleInts := make(model.Roles, 0, len(roleIDs))
		for _, id := range roleIDs {
			roleInts = append(roleInts, int(id))
		}
		authn := lu.Authn
		if authn == "" {
			authn = "[]"
		}
		u := &model.User{
			ID:         lu.ID,
			Username:   lu.Username,
			PwdHash:    lu.PwdHash,
			PwdTS:      lu.PwdTS,
			Salt:       lu.Salt,
			Password:   lu.Password,
			Role:       roleInts,
			Disabled:   lu.Disabled,
			Permission: lu.Permission,
			OtpSecret:  lu.OtpSecret,
			SsoID:      lu.SsoID,
			Authn:      authn,
		}
		if err := db.CreateUser(u); err != nil {
			return errors.Wrapf(err, "migrate v4: carrying user %q", lu.Username)
		}
		if err := db.SetUserRoles(u.ID, roleIDs); err != nil {
			return errors.Wrapf(err, "migrate v4: binding roles for user %q", lu.Username)
		}
	}
	return nil
}

// writeStorages carries storages verbatim. A storage whose driver is not
// registered in this v4 build is imported disabled so it never fails to load;
// the report lists it for the admin to reconfigure or install the plugin.
func (p *plan) writeStorages() error {
	for _, ls := range p.src.storages {
		st := &model.Storage{
			ID:              ls.ID,
			MountPath:       ls.MountPath,
			Order:           ls.Order,
			Driver:          ls.Driver,
			CacheExpiration: ls.CacheExpiration,
			Status:          ls.Status,
			Addition:        ls.Addition,
			Remark:          ls.Remark,
			Modified:        ls.Modified,
			Disabled:        ls.Disabled || p.disabledStorage[ls.ID],
			DisableIndex:    ls.DisableIndex,
			EnableSign:      ls.EnableSign,
			Sort: model.Sort{
				OrderBy:        ls.OrderBy,
				OrderDirection: ls.OrderDirection,
				ExtractFolder:  ls.ExtractFolder,
			},
			Proxy: model.Proxy{
				WebProxy:      ls.WebProxy,
				WebdavPolicy:  ls.WebdavPolicy,
				ProxyRange:    ls.ProxyRange,
				DownProxyUrl:  ls.DownProxyUrl,
				DownProxySign: ls.DownProxySign,
			},
		}
		if err := db.CreateStorage(st); err != nil {
			return errors.Wrapf(err, "migrate v4: carrying storage %q", ls.MountPath)
		}
	}
	return nil
}

// writeMetas carries directory metas verbatim. NodeID is left zero;
// op.adoptNodelessAnchors backfills it on the first storage load (it needs live
// storages, which do not exist during migration).
func (p *plan) writeMetas() error {
	for _, lm := range p.src.metas {
		m := &model.Meta{
			ID:        lm.ID,
			Path:      lm.Path,
			Password:  lm.Password,
			PSub:      lm.PSub,
			Write:     lm.Write,
			WSub:      lm.WSub,
			Hide:      lm.Hide,
			HSub:      lm.HSub,
			Readme:    lm.Readme,
			RSub:      lm.RSub,
			Header:    lm.Header,
			HeaderSub: lm.HeaderSub,
		}
		if err := db.CreateMeta(m); err != nil {
			return errors.Wrapf(err, "migrate v4: carrying meta %q", lm.Path)
		}
	}
	return nil
}

// writeSettings carries values for keys that still exist in v4. First boot's
// initSettings fills in each key's metadata and preserves these values.
func (p *plan) writeSettings() error {
	for i := range p.settingsToCarry {
		if err := db.SaveSettingItem(&p.settingsToCarry[i]); err != nil {
			return errors.Wrapf(err, "migrate v4: carrying setting %q", p.settingsToCarry[i].Key)
		}
	}
	return nil
}

// parseScopes converts a v3 role's raw_permission JSON into PermissionScopes.
func parseScopes(raw string) []model.PermissionEntry {
	if raw == "" {
		return nil
	}
	var scopes []model.PermissionEntry
	if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
		return nil
	}
	return scopes
}

// keepOnlyAdmin strips every role binding from a base_path-fenced user except
// the built-in admin role, so no carried role (read-all "general" OR any custom
// v3 role scoped above the fence) can re-open out-of-fence access. Admin is
// preserved because v3 admins bypassed base_path and were never fenced. The
// caller then binds the user's private base_path scope role.
func keepOnlyAdmin(ids []uint, adminID uint) []uint {
	out := ids[:0]
	for _, id := range ids {
		if id == adminID {
			out = append(out, id)
		}
	}
	return out
}

// parseRoleInts parses a v3 role column, tolerating a JSON `[]int` (3.46+) or a
// bare integer (pre-3.46). Mirrors convert_role.go.
func parseRoleInts(raw legacy.RawRole) []int {
	if len(raw) == 0 {
		return nil
	}
	var list []int
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	if single, err := strconv.Atoi(string(raw)); err == nil {
		return []int{single}
	}
	return nil
}
