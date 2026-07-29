package model_test

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
)

// TestJoinPathIgnoresRemovedBasePath pins the post-removal contract of
// (*User).JoinPath: it only FixAndCleanPath's the request path — no BasePath
// prefixing, no reqPath=="/" special case. The runtime file boundary is
// op.Authorize, not this join.
func TestJoinPathIgnoresRemovedBasePath(t *testing.T) {
	u := &model.User{Username: "u"} // no BasePath field anymore
	got, err := u.JoinPath("/x/y")
	if err != nil || got != "/x/y" {
		t.Fatalf("JoinPath(/x/y) = %q,%v; want /x/y,nil", got, err)
	}
	root, _ := u.JoinPath("/")
	if root != "/" {
		t.Fatalf("JoinPath(/) = %q; want /", root)
	}
	// Non-"/"-prefixed input is normalized to a "/"-prefixed path by
	// FixAndCleanPath (not base-joined).
	rel, _ := u.JoinPath("sub")
	if rel != "/sub" {
		t.Fatalf("JoinPath(sub) = %q; want /sub", rel)
	}
	// ".." is collapsed by stdpath.Clean (capped at "/", never overflows above
	// root) rather than the old JoinBasePath's errs.RelativePath. The collapsed
	// result is then gated by op.Authorize — traversal neutralized, RBAC backstop.
	trav, err := u.JoinPath("/sub/../../etc")
	if err != nil || trav != "/etc" {
		t.Fatalf("JoinPath(/sub/../../etc) = %q,%v; want /etc,nil (clean-collapsed, RBAC gates)", trav, err)
	}
}
