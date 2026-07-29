package data

import (
	"os"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database: " + err.Error())
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic("failed to migrate test database: " + err.Error())
	}
	os.Exit(m.Run())
}

// seedAll runs the full bootstrap role sequence the way InitData does: base
// roles first (so admin/guest exist), then presets and the admin backfill.
func seedAll(t *testing.T) {
	t.Helper()
	initRoles()
	initPresetRoles()
	backfillAdminSystem()
}

func boundPerms(t *testing.T, roleName string) []model.Permission {
	t.Helper()
	role, err := op.GetRoleByName(roleName)
	if err != nil {
		t.Fatalf("get role %q: %v", roleName, err)
	}
	ps, err := db.GetPermissionsByRoleIDs([]uint{role.ID})
	if err != nil {
		t.Fatalf("get perms for %q: %v", roleName, err)
	}
	return ps
}

func systemRows(ps []model.Permission) []model.Permission {
	var out []model.Permission
	for _, p := range ps {
		if p.ResourceType == model.ResourceSystem {
			out = append(out, p)
		}
	}
	return out
}

// expectedActionSet reports whether ps contains exactly one row matching the
// resource type + path + effect + recursive with the given action set.
func hasRow(ps []model.Permission, rt, path, effect string, recursive bool, acts ...model.Action) bool {
	for _, p := range ps {
		prt := p.ResourceType
		if prt == "" {
			prt = model.ResourceFS
		}
		if prt != rt || p.Path != path || p.Effect != effect || p.Recursive != recursive {
			continue
		}
		if len(p.Actions) != len(acts) {
			continue
		}
		set := map[model.Action]int{}
		for _, a := range p.Actions {
			set[a]++
		}
		ok := true
		for _, a := range acts {
			if set[a] == 0 {
				ok = false
				break
			}
			set[a]--
		}
		if ok {
			return true
		}
	}
	return false
}

func TestPresetsSeededWithRows(t *testing.T) {
	seedAll(t)

	cases := []struct {
		role      string
		rt        string
		path      string
		recursive bool
		acts      []model.Action
	}{
		{"viewer", model.ResourceFS, "/", true, []model.Action{model.ActRead}},
		{"editor", model.ResourceFS, "/", true, []model.Action{
			model.ActRead, model.ActWrite, model.ActRename, model.ActMove, model.ActCopy, model.ActRemove}},
		{"uploader", model.ResourceFS, "/", true, []model.Action{model.ActRead, model.ActWrite}},
		{"storage-manager", model.ResourceSystem, "storage", false, []model.Action{
			model.SystemAction("storage", "view"), model.SystemAction("storage", "manage")}},
		{"user-manager", model.ResourceSystem, "user", false, []model.Action{
			model.SystemAction("user", "view"), model.SystemAction("user", "manage")}},
	}
	for _, c := range cases {
		role, err := op.GetRoleByName(c.role)
		if err != nil {
			t.Fatalf("preset role %q missing: %v", c.role, err)
		}
		if !role.Protected {
			t.Errorf("preset role %q must be Protected=true", c.role)
		}
		ps := boundPerms(t, c.role)
		if !hasRow(ps, c.rt, c.path, model.EffectAllow, c.recursive, c.acts...) {
			t.Errorf("preset %q missing expected row rt=%s path=%s acts=%v; got %+v",
				c.role, c.rt, c.path, c.acts, ps)
		}
		for _, p := range ps {
			if p.Origin != model.OriginManual {
				t.Errorf("preset %q row %d origin=%q, want manual", c.role, p.ID, p.Origin)
			}
		}
	}
}

func TestPresetSeedIdempotent(t *testing.T) {
	// Run the full seeding several times; row content and role identity must
	// converge to exactly one copy each — no duplicate rows, no extra roles.
	seedAll(t)
	seedAll(t)
	seedAll(t)

	presets := []string{"viewer", "editor", "uploader", "storage-manager", "user-manager"}
	for _, name := range presets {
		if n := len(boundPerms(t, name)); n != 1 {
			t.Errorf("preset %q has %d bound rows after 3 runs, want exactly 1", name, n)
		}
		// exactly one role of this name
		r, err := db.GetRoleByName(name)
		if err != nil || r == nil {
			t.Errorf("preset role %q not found: %v", name, err)
		}
	}

	// admin system rows must also be stable across runs (one per system domain).
	adminSys := systemRows(boundPerms(t, "admin"))
	wantDomains := systemDomainCount(t)
	if len(adminSys) != wantDomains {
		t.Errorf("admin has %d system rows after 3 runs, want %d (one per domain)", len(adminSys), wantDomains)
	}
}

func TestPartialSeedRecovery(t *testing.T) {
	// Simulate a crash that created the preset role but never seeded its
	// permission rows: strip viewer's rows, then re-run. Row-level (not
	// role-level) idempotency must re-add the missing row.
	seedAll(t)
	role, err := op.GetRoleByName("viewer")
	if err != nil {
		t.Fatalf("viewer missing: %v", err)
	}
	ids, err := db.GetPermissionIDsByRole(role.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := db.DeletePermissionById(id); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(boundPerms(t, "viewer")); n != 0 {
		t.Fatalf("setup: viewer should have 0 rows, got %d", n)
	}

	initPresetRoles()

	ps := boundPerms(t, "viewer")
	if !hasRow(ps, model.ResourceFS, "/", model.EffectAllow, true, model.ActRead) {
		t.Fatalf("partial recovery failed to re-add viewer row; got %+v", ps)
	}
}

func TestAdminSystemBackfill(t *testing.T) {
	seedAll(t)

	adminSys := systemRows(boundPerms(t, "admin"))
	byDomain := map[string]model.Permission{}
	for _, p := range adminSys {
		byDomain[p.Path] = p
	}

	snap, err := op.ActionCatalog()
	if err != nil {
		t.Fatal(err)
	}
	// group registered system actions by domain
	want := map[string][]model.Action{}
	for _, a := range snap.Actions {
		if a.Orphaned || a.ResourceType != model.ResourceSystem || a.Domain == "" {
			continue
		}
		want[a.Domain] = append(want[a.Domain], a.Name)
	}
	if len(want) == 0 {
		t.Fatal("no system domains registered in catalog")
	}
	for dom, acts := range want {
		p, ok := byDomain[dom]
		if !ok {
			t.Errorf("admin missing system row for domain %q", dom)
			continue
		}
		if p.Effect != model.EffectAllow || p.Origin != model.OriginManual {
			t.Errorf("admin domain %q row effect=%q origin=%q, want allow/manual", dom, p.Effect, p.Origin)
		}
		for _, a := range acts {
			if !model.Actions(p.Actions).Contains(a) {
				t.Errorf("admin domain %q row missing action %q; got %v", dom, a, p.Actions)
			}
		}
	}
}

func systemDomainCount(t *testing.T) int {
	t.Helper()
	snap, err := op.ActionCatalog()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]struct{}{}
	for _, a := range snap.Actions {
		if a.Orphaned || a.ResourceType != model.ResourceSystem || a.Domain == "" {
			continue
		}
		seen[a.Domain] = struct{}{}
	}
	return len(seen)
}
