package op_test

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// 建 role + 手工 permission 绑定的夹具
func makePolicyUser(t *testing.T, name string, perms []*model.Permission) *model.User {
	t.Helper()
	r := &model.Role{Name: name + "-role"}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })
	for _, p := range perms {
		if err := db.CreatePermission(p); err != nil {
			t.Fatal(err)
		}
		pid := p.ID
		t.Cleanup(func() { _ = db.DeletePermissionById(pid) })
		if err := db.AddRolePermission(r.ID, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	op.InvalidateRolePolicyForTest(r.ID)
	return &model.User{ID: 9999, Username: name, Role: model.Roles{int(r.ID)}}
}

func TestPolicyDenyOverridesAllow(t *testing.T) {
	u := makePolicyUser(t, "deny1", []*model.Permission{
		{Name: "allow-a", Path: "/a", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
		{Name: "deny-private", Path: "/a/private", Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true},
	})
	d, err := op.ResolveEffectivePolicy(u, "/a/x")
	if err != nil || !d.Can(model.ActRead) {
		t.Fatalf("/a/x should be readable: %v %v", d, err)
	}
	d, _ = op.ResolveEffectivePolicy(u, "/a/private/secret")
	if d.Can(model.ActRead) {
		t.Fatal("deny must override inherited allow")
	}
}

func TestPolicyRecursiveAndExact(t *testing.T) {
	u := makePolicyUser(t, "rec1", []*model.Permission{
		{Name: "exact", Path: "/only", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: false},
	})
	if d, _ := op.ResolveEffectivePolicy(u, "/only"); !d.Can(model.ActRead) {
		t.Fatal("exact path must match")
	}
	if d, _ := op.ResolveEffectivePolicy(u, "/only/sub"); d.Can(model.ActRead) {
		t.Fatal("non-recursive must not cover children")
	}
}

func TestPolicyTraverseAncestors(t *testing.T) {
	u := makePolicyUser(t, "trav1", []*model.Permission{
		{Name: "deep", Path: "/a/b/c", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
	})
	d, _ := op.ResolveEffectivePolicy(u, "/a/b")
	if d.Can(model.ActRead) || !d.Traverse {
		t.Fatalf("ancestor: read=false traverse=true, got %v", d)
	}
	if d, _ := op.ResolveEffectivePolicy(u, "/z"); d.Can(model.ActRead) || d.Traverse {
		t.Fatal("unrelated path must be default-deny")
	}
}

func TestPolicyNonRecursiveDenyExactOnly(t *testing.T) {
	u := makePolicyUser(t, "nrdeny1", []*model.Permission{
		{Name: "allow-nr", Path: "/nr", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
		{Name: "deny-nr-x", Path: "/nr/x", Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: false},
	})
	if d, _ := op.ResolveEffectivePolicy(u, "/nr/x"); d.Can(model.ActRead) {
		t.Fatal("exact-path non-recursive deny must apply at /nr/x")
	}
	if d, _ := op.ResolveEffectivePolicy(u, "/nr/x/child"); !d.Can(model.ActRead) {
		t.Fatal("non-recursive deny must not leak to children of /nr/x")
	}
	if d, _ := op.ResolveEffectivePolicy(u, "/nr/y"); !d.Can(model.ActRead) {
		t.Fatal("unrelated sibling /nr/y must remain allowed")
	}
}

func TestPolicyMultiRoleUnionAndDefaultDeny(t *testing.T) {
	u1 := makePolicyUser(t, "mr1", []*model.Permission{
		{Name: "r", Path: "/m", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
	})
	u2 := makePolicyUser(t, "mr2", []*model.Permission{
		{Name: "w", Path: "/m", Actions: model.Actions{model.ActWrite}, Effect: model.EffectAllow, Recursive: true},
	})
	both := &model.User{ID: 9998, Username: "both",
		Role: append(append(model.Roles{}, u1.Role...), u2.Role...)}
	d, _ := op.ResolveEffectivePolicy(both, "/m/f")
	if !d.Can(model.ActRead) || !d.Can(model.ActWrite) {
		t.Fatalf("multi-role union broken: %v", d)
	}
	if d.Can(model.ActRemove) {
		t.Fatal("unmentioned action must default-deny")
	}
	if d, _ := op.ResolveEffectivePolicy(nil, "/m"); d.Can(model.ActRead) || d.Traverse {
		t.Fatal("nil user must deny")
	}
}

func TestDecisionDeniedNilSafeAndMembership(t *testing.T) {
	var nilDecision *op.Decision
	if nilDecision.Denied(model.ActRead) {
		t.Fatal("nil Decision must report Denied=false")
	}
	u := makePolicyUser(t, "denied1", []*model.Permission{
		{Name: "deny-read", Path: "/dn", Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true},
	})
	d, _ := op.ResolveEffectivePolicy(u, "/dn/x")
	if !d.Denied(model.ActRead) {
		t.Fatal("explicit deny-read must report Denied=true")
	}
	if d.Denied(model.ActWrite) {
		t.Fatal("action with no deny row must report Denied=false")
	}
}

func TestPolicyCacheInvalidation(t *testing.T) {
	p := &model.Permission{Name: "inv", Path: "/inv", Actions: model.Actions{model.ActRead},
		Effect: model.EffectAllow, Recursive: true}
	u := makePolicyUser(t, "inv1", []*model.Permission{p})
	if d, _ := op.ResolveEffectivePolicy(u, "/inv/x"); !d.Can(model.ActRead) {
		t.Fatal("precondition")
	}
	// 改成 deny 并失效缓存 → 决策必须翻转
	p.Effect = model.EffectDeny
	if err := db.UpdatePermission(p); err != nil {
		t.Fatal(err)
	}
	op.InvalidateRolePolicyForTest(uint(u.Role[0]))
	if d, _ := op.ResolveEffectivePolicy(u, "/inv/x"); d.Can(model.ActRead) {
		t.Fatal("stale cache after invalidation")
	}
}
