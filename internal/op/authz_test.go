package op_test

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/pkg/errors"
)

func makeAuthzUser(t *testing.T, name string, perms []*model.Permission) *model.User {
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
		_ = db.AddRolePermission(r.ID, p.ID)
	}
	op.InvalidateRolePolicyForTest(r.ID)
	return &model.User{ID: 7777, Username: name, Role: model.Roles{int(r.ID)}}
}

func mustMeta(t *testing.T, m *model.Meta) {
	t.Helper()
	if err := op.CreateMeta(m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteMetaById(m.ID) })
}

func TestAuthorizeReadPasswordAndHide(t *testing.T) {
	ctx := context.Background()
	u := makeAuthzUser(t, "az1", []*model.Permission{
		{Name: "all", Path: "/az", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
	})
	mustMeta(t, &model.Meta{Path: "/az/locked", Password: "pw", PSub: true})
	mustMeta(t, &model.Meta{Path: "/az/h", Hide: "secret.*", HSub: true})

	if err := op.Authorize(ctx, u, model.ActRead, op.AuthzRequest{Path: "/az/open"}); err != nil {
		t.Fatalf("plain read: %v", err)
	}
	err := op.Authorize(ctx, u, model.ActRead, op.AuthzRequest{Path: "/az/locked/f"})
	if !errors.Is(err, errs.WrongPassword) {
		t.Fatalf("want WrongPassword, got %v", err)
	}
	if err := op.Authorize(ctx, u, model.ActRead,
		op.AuthzRequest{Path: "/az/locked/f", Password: "pw"}); err != nil {
		t.Fatalf("correct password: %v", err)
	}
	err = op.Authorize(ctx, u, model.ActRead, op.AuthzRequest{Path: "/az/h/secret.txt"})
	if !errors.Is(err, errs.PermissionDenied) {
		t.Fatalf("hidden file must deny: %v", err)
	}
	// see_hides 越过 hide
	u2 := makeAuthzUser(t, "az1b", []*model.Permission{
		{Name: "all+hides", Path: "/az", Actions: model.Actions{model.ActRead, model.ActSeeHides},
			Effect: model.EffectAllow, Recursive: true},
	})
	if err := op.Authorize(ctx, u2, model.ActRead, op.AuthzRequest{Path: "/az/h/secret.txt"}); err != nil {
		t.Fatalf("see_hides must bypass hide: %v", err)
	}
}

func TestAuthorizeWriteFallbackToMetaWrite(t *testing.T) {
	ctx := context.Background()
	u := makeAuthzUser(t, "az2", []*model.Permission{
		{Name: "ro", Path: "/az2", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
	})
	mustMeta(t, &model.Meta{Path: "/az2/up", Write: true, WSub: true})
	if err := op.Authorize(ctx, u, model.ActWrite, op.AuthzRequest{Path: "/az2/up/new"}); err != nil {
		t.Fatalf("meta write fallback: %v", err)
	}
	err := op.Authorize(ctx, u, model.ActWrite, op.AuthzRequest{Path: "/az2/ro"})
	if !errors.Is(err, errs.PermissionDenied) {
		t.Fatalf("no write anywhere: %v", err)
	}
	if err := op.Authorize(ctx, u, model.ActRemove, op.AuthzRequest{Path: "/az2/up/new"}); !errors.Is(err, errs.PermissionDenied) {
		t.Fatalf("meta write must not grant remove: %v", err)
	}
}

// An explicit deny-write permission must not be overridden by the directory
// meta.Write fallback: deny is a stronger signal than the "open upload dir"
// convenience escape.
func TestAuthorizeWriteDenyBeatsMetaWriteFallback(t *testing.T) {
	ctx := context.Background()
	u := makeAuthzUser(t, "az4", []*model.Permission{
		{Name: "deny-write", Path: "/az4/up", Actions: model.Actions{model.ActWrite}, Effect: model.EffectDeny, Recursive: true},
	})
	mustMeta(t, &model.Meta{Path: "/az4/up", Write: true, WSub: true})
	err := op.Authorize(ctx, u, model.ActWrite, op.AuthzRequest{Path: "/az4/up/new"})
	if !errors.Is(err, errs.PermissionDenied) {
		t.Fatalf("explicit deny-write must beat meta.Write fallback: %v", err)
	}
}

// A recursive deny-read must beat the Traverse escape: an exact-match allow
// on a deeper path must not resurrect access to a path already covered by a
// recursive deny above it.
func TestAuthorizeReadDenyBeatsTraverseEscape(t *testing.T) {
	ctx := context.Background()
	u := makeAuthzUser(t, "az5", []*model.Permission{
		{Name: "deny-read", Path: "/dp", Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true},
		{Name: "allow-deep", Path: "/dp/deep", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: false},
		{Name: "allow-dp2", Path: "/dp2/deep", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: false},
	})
	err := op.Authorize(ctx, u, model.ActRead, op.AuthzRequest{Path: "/dp/deep"})
	if !errors.Is(err, errs.PermissionDenied) {
		t.Fatalf("recursive deny-read must beat traverse escape at exact-match allow: %v", err)
	}
	if err := op.Authorize(ctx, u, model.ActRead, op.AuthzRequest{Path: "/dp2/deep"}); err != nil {
		t.Fatalf("sibling path without deny must still be readable: %v", err)
	}
}

func TestFilterListingCentralized(t *testing.T) {
	ctx := context.Background()
	u := makeAuthzUser(t, "az3", []*model.Permission{
		{Name: "sub-only", Path: "/az3/visible", Actions: model.Actions{model.ActRead}, Effect: model.EffectAllow, Recursive: true},
		{Name: "deny-x", Path: "/az3/visible/x", Actions: model.Actions{model.ActRead}, Effect: model.EffectDeny, Recursive: true},
	})
	mustMeta(t, &model.Meta{Path: "/az3/visible", Hide: "hid.*", HSub: false})
	objs := []model.Obj{
		&model.Object{Name: "a", IsFolder: true},
		&model.Object{Name: "x", IsFolder: true},        // deny 规则 → 滤掉
		&model.Object{Name: "hidden1", IsFolder: false}, // 不匹配 hid.* 前缀? 匹配 → 滤掉
	}
	got := op.FilterListing(ctx, u, "/az3/visible", objs)
	if len(got) != 1 || got[0].GetName() != "a" {
		names := make([]string, 0, len(got))
		for _, o := range got {
			names = append(names, o.GetName())
		}
		t.Fatalf("want [a], got %v", names)
	}
	// 目录本身无权 → 全滤
	if got := op.FilterListing(ctx, u, "/az3/other", objs); len(got) != 0 {
		t.Fatalf("no-permission dir must filter everything: %d", len(got))
	}
}
