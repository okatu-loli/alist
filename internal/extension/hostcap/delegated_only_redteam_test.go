package hostcap

// F1 red-team: PublicPage / delegated-only Host 越权 suite.
//
// The PublicPage security story (design §2.5) is: a public (unauthenticated)
// route handler gets a Host built with Options.DelegatedOnly, whose FS REFUSES
// the plugin's own bound/self principal (the empty ext.Principal). A public
// handler may therefore act ONLY through an explicitly delegated principal it
// obtained from Auth().DelegatedPrincipal — it can never bare-call host.FS with
// the empty principal to read the files the plugin itself was granted.
//
// delegated_only_test.go already proves List/Mkdir/Remove are refused. The
// attacks below close the remaining surface: EVERY FS verb (not just three),
// the Resolve→Get ref-laundering path, and a forged non-empty principal handle.
// All are expected to be safely denied; they are permanent regressions.

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// publicGrantHost builds a delegated-only Host over a granted+seeded mount, as a
// public route handler would receive it.
func publicGrantHost(t *testing.T) (*Host, string) {
	t.Helper()
	mnt := "/rt-deleg-" + t.Name()
	mountLocal(t, mnt)
	grant := Grant{
		PluginID: "p.public.redteam",
		HostAPIs: []GrantItem{{Name: CapFS}},
		FS:       []FSScope{{Path: mnt, Write: true}},
	}
	return New(grant, Options{DelegatedOnly: true}), mnt
}

// TestReg_DelegatedOnlyRefusesSelfPrincipalEveryVerb is the full-surface version
// of delegated_only_test.go: a public handler's self ("") principal is refused
// by Resolve/List/Get/Link/Mkdir/Move/Rename/Copy/Remove/Put — not a subset.
// A single verb that honored the self principal would be a越权 backdoor letting
// an anonymous caller read/mutate the plugin's granted subtree.
func TestReg_DelegatedOnlyRefusesSelfPrincipalEveryVerb(t *testing.T) {
	h, mnt := publicGrantHost(t)
	ctx := context.Background()
	fsy := h.FS()
	dir := ext.ObjRef{Anchor: mnt}
	file := ext.ObjRef{Anchor: mnt + "/seed.txt"}

	check := func(name string, err error) {
		if err != ext.ErrPermissionDenied {
			t.Fatalf("delegated-only self principal %s: want ErrPermissionDenied, got %v", name, err)
		}
	}

	_, err := fsy.Resolve(ctx, "", mnt)
	check("Resolve", err)
	_, err = fsy.List(ctx, "", dir, ext.ListArgs{})
	check("List", err)
	_, err = fsy.Get(ctx, "", file)
	check("Get", err)
	_, err = fsy.Link(ctx, "", file, ext.LinkArgs{})
	check("Link", err)
	_, err = fsy.Mkdir(ctx, "", dir, "x")
	check("Mkdir", err)
	check("Move", fsy.Move(ctx, "", file, dir))
	check("Rename", fsy.Rename(ctx, "", file, "renamed.txt"))
	check("Copy", fsy.Copy(ctx, "", file, dir))
	check("Remove", fsy.Remove(ctx, "", file))
	_, err = fsy.Put(ctx, "", dir, nil)
	check("Put", err)
}

// TestReg_DelegatedOnlyResolveThenGetLaundering closes the ref-laundering path:
// an attacker might hope Resolve mints a usable ObjRef under the self principal
// that a later Get would honor. Resolve itself must deny the self principal, so
// there is no ref to launder; and even a fabricated ObjRef anchored inside the
// grant is denied by Get because Get re-runs the self-principal refusal.
func TestReg_DelegatedOnlyResolveThenGetLaundering(t *testing.T) {
	h, mnt := publicGrantHost(t)
	ctx := context.Background()
	fsy := h.FS()

	if _, err := fsy.Resolve(ctx, "", mnt+"/seed.txt"); err != ext.ErrPermissionDenied {
		t.Fatalf("Resolve self principal: want denied, got %v", err)
	}
	// Fabricate the ref Resolve would have returned and try to cash it in.
	forged := ext.ObjRef{Anchor: mnt + "/seed.txt"}
	if _, err := fsy.Get(ctx, "", forged); err != ext.ErrPermissionDenied {
		t.Fatalf("Get fabricated ref under self principal: want denied, got %v", err)
	}
}

// TestReg_DelegatedOnlyForgedNonEmptyPrincipalDenied covers the "non-empty but
// bogus handle" evasion: DelegatedOnly refuses the EMPTY principal, so an
// attacker might try a non-empty string hoping it dodges the empty-check and
// falls through to the plugin's bound grant. A non-empty handle is looked up in
// the per-Host principals map (empty for a fresh public Host), misses, and is
// denied — it never resolves to the bound principal.
func TestReg_DelegatedOnlyForgedNonEmptyPrincipalDenied(t *testing.T) {
	h, mnt := publicGrantHost(t)
	ctx := context.Background()
	for _, forged := range []ext.Principal{
		"self", "bound", "0", " ",
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef00",
	} {
		if _, err := h.FS().List(ctx, forged, ext.ObjRef{Anchor: mnt}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
			t.Fatalf("delegated-only forged principal %q: want ErrPermissionDenied, got %v", forged, err)
		}
	}
}
