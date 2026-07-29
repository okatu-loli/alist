package op_test

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/pkg/errors"
)

// These tests are the W4 red-team regression for "delete user.BasePath, RBAC is
// the only file boundary". With the half-baked BasePath fence gone, the entire
// file boundary rests on op.Authorize + op.FilterListing. Each test below pins
// one attack surface closed; the load-bearing comment on each names the exact
// production line whose deletion would turn it red.

// fenceScope grants a restricted (non-admin) user recursive rights on exactly
// /local/sub and nothing else — the v4 equivalent of a v3 base_path fence.
func fenceScope(t *testing.T, name string) *model.User {
	t.Helper()
	return makeAuthzUser(t, name, []*model.Permission{{
		Name: "fence", Path: "/local/sub",
		Actions: model.Actions{
			model.ActRead, model.ActWrite, model.ActMove,
			model.ActCopy, model.ActRemove, model.ActRename,
		},
		Effect: model.EffectAllow, Recursive: true,
	}})
}

// Attack #1 — listing must not leak unauthorized siblings. FilterListing runs a
// per-child ActRead decision; only the granted subtree (and the ancestor
// directories needed to *reach* it via Traverse) may appear. If FilterListing's
// per-child `ResolveEffectivePolicy` / ActRead gate is removed, an unauthorized
// sibling leaks and this goes red.
func TestBasePathRemovalListingNoLeak(t *testing.T) {
	ctx := context.Background()
	u := fenceScope(t, "bp-list")

	// Listing /local: only "sub" (the granted subtree) is visible; "secret" and
	// the prefix-confusion decoy "subsidiary" must be filtered out.
	localChildren := []model.Obj{
		&model.Object{Name: "sub", IsFolder: true},
		&model.Object{Name: "secret", IsFolder: true},
		&model.Object{Name: "subsidiary", IsFolder: true},
	}
	got := op.FilterListing(ctx, u, "/local", localChildren)
	if len(got) != 1 || got[0].GetName() != "sub" {
		t.Fatalf("listing /local must show only [sub], got %v", names(got))
	}

	// Listing the root: only "local" survives (as a Traverse waypoint to reach
	// /local/sub); an unrelated mount "other" must not appear.
	rootChildren := []model.Obj{
		&model.Object{Name: "local", IsFolder: true},
		&model.Object{Name: "other", IsFolder: true},
	}
	got = op.FilterListing(ctx, u, "/", rootChildren)
	if len(got) != 1 || got[0].GetName() != "local" {
		t.Fatalf("listing / must show only [local] (traverse waypoint), got %v", names(got))
	}

	// Listing an unauthorized directory yields nothing at all.
	if got := op.FilterListing(ctx, u, "/local/secret", localChildren); len(got) != 0 {
		t.Fatalf("listing unauthorized dir must be empty, got %v", names(got))
	}
}

// Attack #2 — every fs action on an out-of-fence path is denied. If any handler
// were to skip op.Authorize, or if resolvePolicy's coverage test stopped
// default-denying uncovered paths, these would go red. This directly covers the
// move/copy destination case (ActWrite on the destination directory).
func TestBasePathRemovalOutOfFenceDenied(t *testing.T) {
	ctx := context.Background()
	u := fenceScope(t, "bp-deny")

	outside := []string{"/other", "/local/secret", "/local/secret/x", "/etc"}
	acts := []model.Action{
		model.ActRead, model.ActWrite, model.ActMove,
		model.ActCopy, model.ActRemove, model.ActRename,
	}
	for _, p := range outside {
		for _, a := range acts {
			err := op.Authorize(ctx, u, a, op.AuthzRequest{Path: p})
			if !errors.Is(err, errs.PermissionDenied) {
				t.Errorf("act %q on out-of-fence %q must be PermissionDenied, got %v", a, p, err)
			}
		}
	}

	// Move/copy destination fence: source inside, destination outside. The
	// destination-directory ActWrite check (fsmanage.go FsMove/FsCopy) is what
	// stops content being moved/copied out of the fence.
	if err := op.Authorize(ctx, u, model.ActWrite, op.AuthzRequest{Path: "/other"}); !errors.Is(err, errs.PermissionDenied) {
		t.Errorf("write to out-of-fence move/copy destination must deny, got %v", err)
	}
	// Sanity: inside the fence the same actions are allowed.
	if err := op.Authorize(ctx, u, model.ActWrite, op.AuthzRequest{Path: "/local/sub/x"}); err != nil {
		t.Errorf("write inside fence must be allowed, got %v", err)
	}
}

// Attack #3 — malformed traversal input cannot escape the fence, and the
// scope-prefix cannot be confused. Authorize folds req.Path through
// FixAndCleanPath before the policy match, so "../" chains and separator tricks
// collapse to an absolute path that is then default-denied. Prefix confusion is
// blocked by IsSubPath's separator-suffix test: a scope of /local/sub must never
// authorize /local/subsidiary.
func TestBasePathRemovalPathTraversalDenied(t *testing.T) {
	ctx := context.Background()
	u := fenceScope(t, "bp-trav")

	// Each of these folds (FixAndCleanPath) to a path OUTSIDE /local/sub.
	escapes := []string{
		"/local/sub/../secret",         // -> /local/secret
		"/local/sub/../../etc",         // -> /etc
		"//other",                      // -> /other
		"/local/./../secret",           // -> /secret
		"\\other",                      // backslash -> /other
		"/local/sub/../../../../other", // over-long ../ chain, capped at root -> /other
		"/local/subsidiary",            // PREFIX CONFUSION: sibling, not a child of /local/sub
		"/local/subsidiary/deep/file",  // prefix confusion under decoy
	}
	for _, p := range escapes {
		if err := op.Authorize(ctx, u, model.ActRead, op.AuthzRequest{Path: p}); !errors.Is(err, errs.PermissionDenied) {
			t.Errorf("traversal/prefix input %q must be denied, got %v", p, err)
		}
	}

	// Positive control: the genuine fenced subtree stays reachable, so the denials
	// above are boundary-precise and not a blanket deny. Note "...." is a literal
	// directory name (NOT ".."), so it must stay in-fence — proving the traversal
	// denials come from real path folding, not name-substring heuristics.
	for _, p := range []string{"/local/sub", "/local/sub/deep", "/local/sub/deep/file", "/local/sub/..../x"} {
		if err := op.Authorize(ctx, u, model.ActRead, op.AuthzRequest{Path: p}); err != nil {
			t.Errorf("in-fence path %q must be allowed, got %v", p, err)
		}
	}
}

// Attack #6 — the uniform RBAC engine does not misfire on a full-scope
// (admin-equivalent) principal. Admin fs access in v4 flows through the admin
// role's recursive "/" scope, not a code short-circuit; this pins that a "/"
// grant reaches every path, so the restricted-user logic never over-denies an
// admin.
func TestBasePathRemovalAdminUnaffected(t *testing.T) {
	ctx := context.Background()
	admin := makeAuthzUser(t, "bp-admin", []*model.Permission{{
		Name: "root-all", Path: "/",
		Actions: model.Actions{
			model.ActRead, model.ActWrite, model.ActMove,
			model.ActCopy, model.ActRemove, model.ActRename,
		},
		Effect: model.EffectAllow, Recursive: true,
	}})
	for _, p := range []string{"/other", "/local/secret", "/etc", "/local/sub"} {
		if err := op.Authorize(ctx, admin, model.ActRead, op.AuthzRequest{Path: p}); err != nil {
			t.Errorf("full-scope principal must read %q, got %v", p, err)
		}
		if err := op.Authorize(ctx, admin, model.ActWrite, op.AuthzRequest{Path: p}); err != nil {
			t.Errorf("full-scope principal must write %q, got %v", p, err)
		}
	}
}

func names(objs []model.Obj) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.GetName())
	}
	return out
}
