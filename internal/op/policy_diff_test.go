package op_test

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

func TestPolicyDiffDetectsAncestorPassword(t *testing.T) {
	// /pd/top 有密码+PSub,/pd/top/mid 是空 meta:旧语义(nearest)在 mid 之下不要密码,
	// 新语义(沿链)要——diff 报告必须抓到这条。
	mustMetaForDiff(t, &model.Meta{Path: "/pd/top", Password: "pw", PSub: true})
	mustMetaForDiff(t, &model.Meta{Path: "/pd/top/mid"})

	r := &model.Role{Name: "pd-role", PermissionScopes: []model.PermissionEntry{
		{Path: "/pd", Permission: 0},
	}}
	if err := op.CreateRole(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })
	u := &model.User{ID: 5555, Username: "pd", Role: model.Roles{int(r.ID)}}

	diffs, err := op.PolicyDiffReport(u, []string{"/pd/top/mid/leaf", "/pd/plain"})
	if err != nil {
		t.Fatal(err)
	}
	var hit bool
	for _, d := range diffs {
		if d.Kind == "ancestor-password-now-applies" && d.Path == "/pd/top/mid/leaf" {
			hit = true
		}
		if d.Path == "/pd/plain" {
			t.Errorf("no-change path must not be reported: %+v", d)
		}
	}
	if !hit {
		t.Fatalf("ancestor password diff not detected: %+v", diffs)
	}
}

func mustMetaForDiff(t *testing.T, m *model.Meta) {
	t.Helper()
	if err := op.CreateMeta(m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteMetaById(m.ID) })
}
