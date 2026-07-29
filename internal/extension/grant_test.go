package extension

import (
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// manifestWithPerms clones the valid test manifest and stamps a version +
// permission set, so a grant test can control exactly what a package requests.
func manifestWithPerms(version string, p ext.Permissions) ext.Manifest {
	m := validManifest()
	m.Version = version
	m.Permissions = p
	return m
}

func TestInstallRecordsDefaultGrantFromManifest(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	m := manifestWithPerms("1.0.0", ext.Permissions{
		Network:       []string{"api.example.com"},
		HostAPIs:      []string{"events", "store"},
		StorageTokens: []string{"self"},
		Filesystem:    "temp",
	})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	st, ok := mgr.Get(m.ID)
	if !ok || st.Grant == nil {
		t.Fatalf("install did not record a default Grant: %+v ok=%v", st, ok)
	}
	if st.Grant.ReviewedVersion != "1.0.0" {
		t.Fatalf("grant reviewed version = %q", st.Grant.ReviewedVersion)
	}
	if !st.Grant.HasAPI("events") || !st.Grant.HasAPI("store") {
		t.Fatalf("default grant missing declared host_apis: %+v", st.Grant.HostAPIs)
	}
	if len(st.Grant.Network) != 1 || st.Grant.Network[0] != "api.example.com" {
		t.Fatalf("default grant network = %v", st.Grant.Network)
	}
	if st.Grant.Filesystem != "temp" {
		t.Fatalf("default grant fs = %q", st.Grant.Filesystem)
	}
}

func TestGrantedHostAPIsResolverSemantics(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	// Unknown id (a built-in) → (nil,false) so the gate keeps its own rule.
	if apis, ok := mgr.GrantedHostAPIs("no.such.plugin"); ok || apis != nil {
		t.Fatalf("unknown plugin should resolve (nil,false), got (%v,%v)", apis, ok)
	}

	m := manifestWithPerms("1.0.0", ext.Permissions{HostAPIs: []string{"events"}})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Installed with default grant → authoritative, includes declared api.
	apis, ok := mgr.GrantedHostAPIs(m.ID)
	if !ok {
		t.Fatal("installed plugin should resolve authoritative (ok=true)")
	}
	if !containsStr(apis, "events") {
		t.Fatalf("granted host_apis = %v, want events", apis)
	}

	// Revoke to empty grant → authoritative empty (default-deny), NOT a fallback.
	if err := mgr.SetGrant(m.ID, &Grant{}, false); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	apis, ok = mgr.GrantedHostAPIs(m.ID)
	if !ok {
		t.Fatal("revoked plugin still resolves authoritative (ok=true)")
	}
	if len(apis) != 0 {
		t.Fatalf("revoked grant should expose no host_apis, got %v", apis)
	}
}

func TestSetGrantPersistsAndStampsVersion(t *testing.T) {
	pub, priv := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")
	reg := NewRegistry()
	mgr, err := NewManager(dir, reg, NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	m := manifestWithPerms("1.0.0", ext.Permissions{HostAPIs: []string{"events", "store"}})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := mgr.SetGrant(m.ID, &Grant{HostAPIs: []GrantItem{{Name: "events"}}}, false); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	// Reload from disk: the narrowed grant survives, version-stamped.
	mgr2, err := NewManager(dir, NewRegistry(), NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	st, ok := mgr2.Get(m.ID)
	if !ok || st.Grant == nil {
		t.Fatalf("grant not persisted: %+v ok=%v", st, ok)
	}
	if st.Grant.HasAPI("store") {
		t.Fatal("revoked 'store' capability came back after reload")
	}
	if !st.Grant.HasAPI("events") || st.Grant.ReviewedVersion != "1.0.0" {
		t.Fatalf("persisted grant wrong: %+v", st.Grant)
	}
}

func TestUpdateDoesNotAutoWidenGrant(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	m1 := manifestWithPerms("1.0.0", ext.Permissions{
		Network:  []string{"api.example.com"},
		HostAPIs: []string{"events"},
	})
	if _, err := mgr.Install(buildSignedPackage(t, m1, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Update requests a NEW host_api + a NEW network host.
	m2 := manifestWithPerms("2.0.0", ext.Permissions{
		Network:  []string{"api.example.com", "evil.example.net"},
		HostAPIs: []string{"events", "store"},
	})
	if _, err := mgr.Update(buildSignedPackage(t, m2, priv)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st2, _ := mgr.Get(m1.ID)
	if !st2.NeedsReconsent {
		t.Fatal("update requesting new perms should flag NeedsReconsent")
	}
	// The new capability was NOT auto-granted.
	if st2.Grant.HasAPI("store") {
		t.Fatal("update auto-granted a new host_api without re-consent")
	}
	if containsStr(st2.Grant.Network, "evil.example.net") {
		t.Fatal("update auto-added a new network host without re-consent")
	}
	// The previously approved capability is retained.
	if !st2.Grant.HasAPI("events") {
		t.Fatal("update dropped a previously approved capability")
	}
	// A re-consent that approves the new set clears the flag.
	if err := mgr.SetGrant(m1.ID, &Grant{
		HostAPIs: []GrantItem{{Name: "events"}, {Name: "store"}},
		Network:  []string{"api.example.com", "evil.example.net"},
	}, false); err != nil {
		t.Fatalf("SetGrant re-consent: %v", err)
	}
	st3, _ := mgr.Get(m1.ID)
	if st3.NeedsReconsent {
		t.Fatal("re-consent did not clear NeedsReconsent")
	}
	if st3.Grant.ReviewedVersion != "2.0.0" {
		t.Fatalf("re-consent should stamp active version, got %q", st3.Grant.ReviewedVersion)
	}
}

func TestUpdateDowngradeNeedsNoReconsent(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	m1 := manifestWithPerms("1.0.0", ext.Permissions{HostAPIs: []string{"events", "store"}})
	if _, err := mgr.Install(buildSignedPackage(t, m1, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	m2 := manifestWithPerms("2.0.0", ext.Permissions{HostAPIs: []string{"events"}})
	if _, err := mgr.Update(buildSignedPackage(t, m2, priv)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st, _ := mgr.Get(m1.ID)
	if st.NeedsReconsent {
		t.Fatal("a pure downgrade should not require re-consent")
	}
}

func TestPermissionViewReportsRequestedAndGranted(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	m := manifestWithPerms("1.0.0", ext.Permissions{
		Network:  []string{"api.example.com"},
		HostAPIs: []string{"events", "store"},
	})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Narrow the grant to just events.
	if err := mgr.SetGrant(m.ID, &Grant{HostAPIs: []GrantItem{{Name: "events"}}}, false); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	view, err := mgr.PermissionView(m.ID)
	if err != nil {
		t.Fatalf("PermissionView: %v", err)
	}
	if !containsStr(view.Requested.HostAPIs, "store") {
		t.Fatalf("requested should still show the manifest's 'store': %+v", view.Requested)
	}
	if view.Granted == nil || view.Granted.HasAPI("store") {
		t.Fatalf("granted should reflect the narrowed grant (no 'store'): %+v", view.Granted)
	}
	if !view.Granted.HasAPI("events") {
		t.Fatal("granted should include the approved 'events'")
	}
}

func TestClampToManifest_NetworkOverride(t *testing.T) {
	man := ext.Manifest{Version: "1.0.0", Permissions: ext.Permissions{
		HostAPIs: []string{"store"}, Network: []string{"api.declared.com"}, Filesystem: "temp",
	}}
	// submitted grant asks for an EXTRA host + an off-manifest host_api + wider fs
	g := &Grant{
		HostAPIs:   []GrantItem{{Name: "store"}, {Name: "sign"}}, // sign not declared
		Network:    []string{"api.declared.com", "cdn.extra.com"},
		Filesystem: "full", // wider than temp
	}
	// allowExtraNetwork=false: extra host dropped, off-manifest api dropped, fs capped
	clamped := g.clampToManifest(man, false)
	if got := clamped.Network; len(got) != 1 || got[0] != "api.declared.com" {
		t.Fatalf("clamp(false) network = %v, want [api.declared.com]", got)
	}
	if clamped.HasAPI("sign") {
		t.Fatal("clamp must drop off-manifest host_api 'sign'")
	}
	if clamped.Filesystem != "temp" {
		t.Fatalf("clamp must cap fs to temp, got %q", clamped.Filesystem)
	}
	// allowExtraNetwork=true: extra host KEPT, but api/fs STILL clamped
	over := g.clampToManifest(man, true)
	if !containsStr(over.Network, "cdn.extra.com") || !containsStr(over.Network, "api.declared.com") {
		t.Fatalf("clamp(true) must keep extra network, got %v", over.Network)
	}
	if over.HasAPI("sign") {
		t.Fatal("clamp(true) must STILL drop off-manifest host_api 'sign'")
	}
	if over.Filesystem != "temp" {
		t.Fatalf("clamp(true) must STILL cap fs to temp, got %q", over.Filesystem)
	}
}
