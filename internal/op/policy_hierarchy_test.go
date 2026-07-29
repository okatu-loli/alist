package op

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
)

// decision builds a Decision from bare allow/deny action lists. Internal to
// package op so it can reach the unexported allowed/denied maps — this exercises
// Can's hierarchy logic in isolation, without DB rows or resolvePolicy.
func decision(allow, deny []model.Action) *Decision {
	d := &Decision{allowed: map[model.Action]struct{}{}, denied: map[model.Action]struct{}{}}
	for _, a := range allow {
		d.allowed[a] = struct{}{}
	}
	for _, a := range deny {
		d.denied[a] = struct{}{}
	}
	return d
}

// registerFineToken simulates the v4.1 split of system.setting.token out of
// system.setting.manage: it re-registers manage with an expanded Implies list
// (view + token) and registers the new fine action, then restores the pristine
// core catalog on cleanup. Proving Can works here proves the split is
// non-breaking (spec §6.1/§6.3): no stored row changes, manage still covers
// view, and "manage minus token" becomes expressible.
func registerFineToken(t *testing.T) {
	t.Helper()
	manage := model.SystemAction("setting", "manage")
	view := model.SystemAction("setting", "view")
	token := model.SystemAction("setting", "token")
	if err := RegisterActions("core",
		ActionDef{Name: manage, ResourceType: model.ResourceSystem, Domain: "setting", Verb: "manage",
			Implies: []model.Action{view, token}, I18nKey: "perm.action.system.setting.manage"},
		ActionDef{Name: token, ResourceType: model.ResourceSystem, Domain: "setting", Verb: "token",
			I18nKey: "perm.action.system.setting.token", Danger: true},
	); err != nil {
		t.Fatalf("register fine token: %v", err)
	}
	t.Cleanup(func() {
		// UnregisterBySource("core") + re-register the canonical set (which does
		// NOT contain token) returns the catalog to its pristine init() state.
		UnregisterBySource("core")
		if err := RegisterActions("core", coreActionDefs()...); err != nil {
			t.Fatalf("restore core catalog: %v", err)
		}
	})
}

// TestManageImpliesView: granting the coarse manage action must cover the finer
// view via the catalog's manage⇒view implication (spec §6.1 F4 — no "granted
// manage but GET 403").
func TestManageImpliesView(t *testing.T) {
	manage := model.SystemAction("storage", "manage")
	view := model.SystemAction("storage", "view")
	d := decision([]model.Action{manage}, nil)
	if !d.Can(manage) {
		t.Fatal("manage grant must allow manage itself")
	}
	if !d.Can(view) {
		t.Fatal("manage must imply view")
	}
}

// TestManageMinusFine: allow manage + deny the freshly-split token ⇒ token is
// denied (its own deny wins over the ancestor allow), yet view still resolves
// via the manage grant. This is the "manage minus token" shape (spec §6.2).
func TestManageMinusFine(t *testing.T) {
	registerFineToken(t)
	manage := model.SystemAction("setting", "manage")
	view := model.SystemAction("setting", "view")
	token := model.SystemAction("setting", "token")
	d := decision([]model.Action{manage}, []model.Action{token})
	if d.Can(token) {
		t.Fatal("explicit deny on token must beat the ancestor manage allow")
	}
	if !d.Can(view) {
		t.Fatal("manage must still imply view after the token carve-out")
	}
	if !d.Can(manage) {
		t.Fatal("manage itself must remain allowed")
	}
}

// TestFineAloneGrant: granting only the fine token action covers token but must
// NOT leak upward to its parent manage nor sideways to the sibling view (spec
// §6.2 "仅 token").
func TestFineAloneGrant(t *testing.T) {
	registerFineToken(t)
	manage := model.SystemAction("setting", "manage")
	view := model.SystemAction("setting", "view")
	token := model.SystemAction("setting", "token")
	d := decision([]model.Action{token}, nil)
	if !d.Can(token) {
		t.Fatal("token grant must allow token")
	}
	if d.Can(manage) {
		t.Fatal("a fine grant must not imply its coarse parent")
	}
	if d.Can(view) {
		t.Fatal("a fine grant must not imply a sibling action")
	}
}

// TestHierarchyDenyFirstPerLevel: an ancestor deny must beat a nearer descendant
// allow. allow token (near) + deny manage (ancestor) ⇒ token denied, because the
// deny scan covers the FULL chain before any allow is considered (spec §6.2).
func TestHierarchyDenyFirstPerLevel(t *testing.T) {
	registerFineToken(t)
	manage := model.SystemAction("setting", "manage")
	view := model.SystemAction("setting", "view")
	token := model.SystemAction("setting", "token")
	d := decision([]model.Action{token}, []model.Action{manage})
	if d.Can(token) {
		t.Fatal("ancestor deny (manage) must beat the nearer allow (token)")
	}
	if d.Can(view) {
		t.Fatal("deny on manage must also deny view (its descendant)")
	}
}
