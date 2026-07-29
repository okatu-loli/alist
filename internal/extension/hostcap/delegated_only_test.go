package hostcap

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// TestDelegatedOnlyRefusesSelfPrincipal is the PublicPage security control
// (design §2.5): a Host built for an unauthenticated public handler must NOT let
// the plugin borrow its own bound (self) fs grant. A normal Host lists the
// granted subtree with the empty (self) principal; a delegated-only Host refuses
// it — the public handler can only act through an explicitly delegated
// principal, so a public share page can never be a backdoor to arbitrary files
// the plugin was granted.
func TestDelegatedOnlyRefusesSelfPrincipal(t *testing.T) {
	mnt := "/deleg"
	mountLocal(t, mnt)
	ctx := context.Background()

	grant := Grant{
		PluginID: "p.public",
		HostAPIs: []GrantItem{{Name: CapFS}},
		FS:       []FSScope{{Path: mnt, Write: true}},
	}

	// Normal Host: self principal ("") lists the granted subtree.
	normal := New(grant, Options{})
	if _, err := normal.FS().List(ctx, "", ext.ObjRef{Anchor: mnt}, ext.ListArgs{}); err != nil {
		t.Fatalf("normal Host self-principal List: %v", err)
	}

	// Delegated-only Host: the SAME grant + self principal is refused.
	public := New(grant, Options{DelegatedOnly: true})
	if _, err := public.FS().List(ctx, "", ext.ObjRef{Anchor: mnt}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
		t.Fatalf("delegated-only self-principal List: want ErrPermissionDenied, got %v", err)
	}
	// Every write verb is equally refused for the self principal.
	if _, err := public.FS().Mkdir(ctx, "", ext.ObjRef{Anchor: mnt}, "x"); err != ext.ErrPermissionDenied {
		t.Fatalf("delegated-only self-principal Mkdir: want ErrPermissionDenied, got %v", err)
	}
	if err := public.FS().Remove(ctx, "", ext.ObjRef{Anchor: mnt + "/seed.txt"}); err != ext.ErrPermissionDenied {
		t.Fatalf("delegated-only self-principal Remove: want ErrPermissionDenied, got %v", err)
	}
}
