package extension

import (
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// TestDispatchGateReadsPersistedGrantNotManifest is the core F-consent security
// invariant: once the resolver is wired to a Manager's persisted Grant, a
// plugin's DECLARED capability is denied unless the admin actually APPROVED it,
// and a revocation takes effect immediately.
func TestDispatchGateReadsPersistedGrantNotManifest(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	// The manifest DECLARES both events and store.
	m := manifestWithPerms("1.0.0", ext.Permissions{HostAPIs: []string{"events", "store"}})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Wire the gate to this manager's persisted consent, restoring afterwards so
	// the package-global seam does not leak into other tests.
	prev := currentGrantOverride()
	SetGrantOverride(mgr.GrantedHostAPIs)
	defer SetGrantOverride(prev)

	// An Entry carrying the same declaring manifest — the gate must consult the
	// Grant, not this declaration.
	e := &Entry{Manifest: m}

	// Admin approves ONLY events.
	if err := mgr.SetGrant(m.ID, &Grant{HostAPIs: []GrantItem{{Name: "events"}}}, false); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	if !e.GrantsCapability("events") {
		t.Fatal("approved 'events' should be granted")
	}
	if e.GrantsCapability("store") {
		t.Fatal("declared-but-unapproved 'store' must be DENIED (manifest != granted)")
	}

	// Revoke everything → immediate default-deny, even for the declared caps.
	if err := mgr.SetGrant(m.ID, &Grant{}, false); err != nil {
		t.Fatalf("SetGrant revoke: %v", err)
	}
	if e.GrantsCapability("events") {
		t.Fatal("revocation should take effect immediately")
	}

	// Grant it back → immediate re-enable.
	if err := mgr.SetGrant(m.ID, &Grant{HostAPIs: []GrantItem{{Name: "store"}}}, false); err != nil {
		t.Fatalf("SetGrant regrant: %v", err)
	}
	if !e.GrantsCapability("store") {
		t.Fatal("re-granting 'store' should take effect immediately")
	}
}

// currentGrantOverride snapshots the installed resolver so a test can restore it.
func currentGrantOverride() func(string) ([]string, bool) {
	grantOverrideMu.RLock()
	defer grantOverrideMu.RUnlock()
	return grantOverride
}
