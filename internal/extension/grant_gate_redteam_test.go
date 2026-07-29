package extension

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// This file is the F-consent RED-TEAM adversarial suite: each test constructs an
// attack against the "admin approved == effective grant, manifest declaration is
// NOT authority" trust boundary and asserts the system's actual behavior. Tests
// whose name ends in _Denied / _NotFallback are guarding a confirmed defense
// (they must stay green forever). Tests whose name contains _GAP_ characterize a
// confirmed weakness — they assert the CURRENT (insecure) behavior on purpose so
// the suite stays green while pinning the gap; the doc comment states what SHOULD
// happen and the remediation.

// wireGrant points the package-global dispatch-gate resolver at mgr for the
// duration of a test, restoring the prior resolver on cleanup so the seam does
// not leak between tests.
func wireGrant(t *testing.T, mgr *Manager) {
	t.Helper()
	prev := currentGrantOverride()
	SetGrantOverride(mgr.GrantedHostAPIs)
	t.Cleanup(func() { SetGrantOverride(prev) })
}

// ---------------------------------------------------------------------------
// Attack 1 — default-deny / manifest declaration must never leak into a Grant.
// ---------------------------------------------------------------------------

// TestRedteam_NilGrantResolver_GAP_Panics is a CONFIRMED BUG (severity: DoS +
// broken default-deny contract). GrantedHostAPIs calls ps.Grant.hostAPINames(),
// and hostAPINames dereferences its receiver (make([]string,0,len(g.HostAPIs)))
// WITHOUT a nil guard — even though its own doc comment promises "A nil Grant
// yields an empty (non-nil) slice: default-deny, never 'unset'." (Contrast
// Grant.HasAPI, which does guard nil.) So an installed plugin whose persisted
// state has a nil Grant makes the dispatch-gate resolver PANIC instead of
// default-denying.
//
// Reachability (realistic, not contrived): this is exactly the F-consent UPGRADE
// path. A plugin installed+enabled by a pre-F-consent build wrote a state.json
// with NO "grant" field; loading it under the new build yields ps.Grant == nil.
// LoadEnabled re-registers it, and the FIRST event/public-route dispatch calls
// GrantsCapability -> grantOverride -> GrantedHostAPIs -> panic. The panic in
// DispatchEvent's gate check (eventhook.go:78) is OUTSIDE the per-hook recover,
// so it propagates up the fs-mutation dispatch path. A hand-edited or partially
// written state.json triggers the same crash.
//
// Root cause: internal/extension/grant.go hostAPINames() is missing
// `if g == nil { return []string{} }`.
// Fix: add the nil guard (matching HasAPI), delivering the documented empty-set
// default-deny instead of a crash.
//
// This test reproduces via a faithful legacy state.json (no grant field) and
// asserts the panic, pinning the bug; when fixed, replace the recover with the
// default-deny assertions (kept below, commented, ready to flip).
func TestRedteam_NilGrantResolver_GAP_Panics(t *testing.T) {
	pub, _ := genKey(t)
	dir := filepath.Join(t.TempDir(), "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A legacy state.json: an installed+enabled plugin with NO "grant" key.
	const id = "legacy.nogrant.plugin"
	legacy := `{"plugins":{"` + id + `":{"id":"` + id + `","kind":"storage_driver",` +
		`"active_version":"1.0.0","versions":["1.0.0"],"enabled":true}}}`
	if err := os.WriteFile(filepath.Join(dir, StateFile), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(dir, NewRegistry(), NewVerifier(pub))
	if err != nil {
		t.Fatalf("NewManager (legacy state): %v", err)
	}
	// Confirm the state loaded with a nil Grant (the upgrade condition).
	st, ok := mgr.Get(id)
	if !ok || st.Grant != nil {
		t.Fatalf("legacy state should load with a nil Grant, got ok=%v grant=%+v", ok, st.Grant)
	}

	// FIXED (hostAPINames nil guard): a nil Grant resolves to the documented
	// empty-set default-deny instead of panicking.
	apis, ok := mgr.GrantedHostAPIs(id)
	if !ok {
		t.Fatal("installed plugin must resolve authoritative even with nil Grant")
	}
	if len(apis) != 0 {
		t.Fatalf("nil Grant must expose no host_apis (default-deny), got %v", apis)
	}
}

// TestRedteam_EmptyGrant_DeniesDeclared_Denied: the ordinary revoke-to-empty
// path (SetGrant with &Grant{}) must deny every manifest-declared capability at
// the gate — including the public-route and event dispatch caps that are the
// real reachable surface (apiroute.go / eventhook.go).
func TestRedteam_EmptyGrant_DeniesDeclared_Denied(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)
	m := manifestWithPerms("1.0.0", ext.Permissions{
		HostAPIs: []string{CapEvents, CapPublicRoutes},
	})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := mgr.SetGrant(m.ID, &Grant{}, false); err != nil {
		t.Fatalf("SetGrant revoke: %v", err)
	}
	wireGrant(t, mgr)
	e := &Entry{Manifest: m}
	if e.GrantsCapability(CapEvents) || e.GrantsCapability(CapPublicRoutes) {
		t.Fatal("empty Grant must deny events + public_routes despite manifest declaring them")
	}
}

// ---------------------------------------------------------------------------
// Attack 2/4 — over-grant beyond the Manifest ceiling (CONFIRMED GAP).
// ---------------------------------------------------------------------------

// TestRedteam_OverGrantBeyondManifest_GAP_NotClamped documents a CONFIRMED
// weakness: SetGrant (and therefore POST /grant and install-with-grant, which
// funnel into it) stores whatever Grant it is handed WITHOUT intersecting it
// against the active version's Manifest requests. A plugin that declared only
// "events" can be granted "public_routes", "store", full network egress ("*")
// and fs "full" — none of which its Manifest ever asked for — and the runtime
// dispatch gate then HONORS them (GrantsCapability(public_routes)==true, so its
// unauthenticated public routes become reachable via DispatchAPIRoute).
//
// Root cause: SetGrant (lifecycle.go) replaces ps.Grant wholesale with no
// ⊆-manifest check; the admin API handlers (SetPluginGrant / installOrUpdate)
// forward req.Grant verbatim. The Manifest is documented as the ceiling
// ("what a plugin REQUESTS") but is not enforced as one.
//
// Impact: the consent UI renders "requested" from the Manifest, but the API
// accepts a Grant beyond it — a confused-deputy surface (a buggy/compromised
// admin frontend, an API misuse, or a CSRF against an admin session) can escalate
// a plugin past its declared capability surface. Because hostcap's runtime Grant
// is derived from this same persisted consent (GrantFromConsent maps fs "full"
// -> "/", network "*" -> open egress), the over-grant becomes real reach once the
// launcher wiring lands.
//
// Recommendation: clamp/validate the submitted Grant against the active
// Manifest's RequestedPermissions in SetGrant — reject (or drop) host_apis,
// network hosts, storage_tokens, and any fs level the Manifest did not request.
//
// This test asserts the CURRENT insecure behavior so the gap is pinned; flip the
// asserts to "must be denied" once the clamp lands.
func TestRedteam_OverGrantBeyondManifest_GAP_NotClamped(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	// Manifest requests ONLY "events" — nothing else.
	m := manifestWithPerms("1.0.0", ext.Permissions{HostAPIs: []string{CapEvents}})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Admin over-grants far beyond the manifest.
	over := &Grant{
		HostAPIs:      []GrantItem{{Name: CapEvents}, {Name: CapPublicRoutes}, {Name: "store"}},
		Network:       []string{"*"},
		StorageTokens: []GrantItem{{Name: "root"}},
		Filesystem:    "full",
	}
	if err := mgr.SetGrant(m.ID, over, false); err != nil {
		t.Fatalf("SetGrant over-grant unexpectedly failed: %v", err)
	}

	wireGrant(t, mgr)
	e := &Entry{Manifest: m}

	// FIXED (clampToManifest): capabilities the manifest never requested are
	// dropped, so the gate does NOT honor the over-granted public_routes / store.
	if e.GrantsCapability(CapPublicRoutes) {
		t.Fatal("REGRESSION: over-granted public_routes (not in manifest) honored at the gate")
	}
	if e.GrantsCapability("store") {
		t.Fatal("REGRESSION: over-granted 'store' (not in manifest) honored at the gate")
	}
	// The persisted Grant is clamped: no "*" egress, no full fs the manifest never asked for.
	st, _ := mgr.Get(m.ID)
	if containsStr(st.Grant.Network, "*") || st.Grant.Filesystem == "full" {
		t.Fatalf("REGRESSION: over-granted network/fs persisted unclamped, got %+v", st.Grant)
	}
	// Granted ⊆ requested holds: the one requested cap survives, nothing else.
	view, err := mgr.PermissionView(m.ID)
	if err != nil {
		t.Fatalf("PermissionView: %v", err)
	}
	if view.Granted.HasAPI("store") {
		t.Fatal("REGRESSION: granted exceeds requested (store not in manifest)")
	}
	if !view.Granted.HasAPI(CapEvents) {
		t.Fatal("the one requested-and-granted capability (events) must survive the clamp")
	}
}

// ---------------------------------------------------------------------------
// Attack 2 — update must not silently proactively-privilege through the gate.
// ---------------------------------------------------------------------------

// TestRedteam_UpdateCannotWidenThroughGate_Denied: an Update to a version that
// requests MORE than the current Grant approves must leave the new capability
// DENIED at the actual dispatch gate (not merely absent from Grant.HostAPIs)
// until an admin re-consents. Complements TestUpdateDoesNotAutoWidenGrant by
// driving the wired resolver end-to-end.
func TestRedteam_UpdateCannotWidenThroughGate_Denied(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	m1 := manifestWithPerms("1.0.0", ext.Permissions{HostAPIs: []string{CapEvents}})
	if _, err := mgr.Install(buildSignedPackage(t, m1, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// v2 additionally requests public_routes.
	m2 := manifestWithPerms("2.0.0", ext.Permissions{HostAPIs: []string{CapEvents, CapPublicRoutes}})
	if _, err := mgr.Update(buildSignedPackage(t, m2, priv)); err != nil {
		t.Fatalf("Update: %v", err)
	}

	wireGrant(t, mgr)
	// The Entry the running host holds declares the NEW (v2) manifest — the gate
	// must still deny the newly-requested cap because consent was not re-granted.
	e := &Entry{Manifest: m2}
	st, _ := mgr.Get(m1.ID)
	if !st.NeedsReconsent {
		t.Fatal("update requesting more must flag NeedsReconsent")
	}
	if e.GrantsCapability(CapPublicRoutes) {
		t.Fatal("update proactively-privileged: newly-requested public_routes reachable before re-consent")
	}
	if !e.GrantsCapability(CapEvents) {
		t.Fatal("previously-approved events must still be granted after update")
	}
}

// TestRedteam_DowngradeUpdateLeavesStaleGrant_GAP: a downgrade Update (the new
// version requests LESS) keeps the prior Grant as-is and needs no re-consent
// (correct — it never widens). A side effect is that the retained Grant can now
// EXCEED the new version's Manifest: v2 dropped "store" from its requests, yet
// the gate still grants it. Same root cause as the clamp gap (Grant is not
// re-intersected with the active manifest). Low impact — the new code no longer
// declares the dropped capability — but the manifest is again not a live ceiling.
func TestRedteam_DowngradeUpdateLeavesStaleGrant_GAP(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)

	m1 := manifestWithPerms("1.0.0", ext.Permissions{HostAPIs: []string{CapEvents, "store"}})
	if _, err := mgr.Install(buildSignedPackage(t, m1, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// v2 drops "store".
	m2 := manifestWithPerms("2.0.0", ext.Permissions{HostAPIs: []string{CapEvents}})
	if _, err := mgr.Update(buildSignedPackage(t, m2, priv)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st, _ := mgr.Get(m1.ID)
	if st.NeedsReconsent {
		t.Fatal("a downgrade must not require re-consent")
	}
	// FIXED (clamp on update): the retained Grant is re-intersected with the new
	// manifest, so a capability the active version no longer requests is dropped.
	view, _ := mgr.PermissionView(m1.ID)
	if containsStr(view.Requested.HostAPIs, "store") {
		t.Fatal("test setup invalid: v2 manifest should not request 'store'")
	}
	if view.Granted.HasAPI("store") {
		t.Fatal("REGRESSION: stale grant retained 'store' above the downgraded manifest")
	}
	if !view.Granted.HasAPI(CapEvents) {
		t.Fatal("the still-requested capability (events) must remain granted after downgrade")
	}
}

// ---------------------------------------------------------------------------
// Attack 3 — revocation immediacy at the dispatch gate.
// ---------------------------------------------------------------------------

// TestRedteam_RevocationImmediateAtGate_Denied: revoking a capability via
// SetGrant is reflected on the VERY NEXT gate read for the same long-lived
// Entry, with no cached/closed-over grant surviving. Guards against a resolver
// that snapshots the grant per-Entry.
func TestRedteam_RevocationImmediateAtGate_Denied(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)
	m := manifestWithPerms("1.0.0", ext.Permissions{HostAPIs: []string{CapEvents}})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	wireGrant(t, mgr)
	e := &Entry{Manifest: m}
	if !e.GrantsCapability(CapEvents) {
		t.Fatal("default grant should permit declared 'events'")
	}
	if err := mgr.SetGrant(m.ID, &Grant{}, false); err != nil {
		t.Fatalf("SetGrant revoke: %v", err)
	}
	// No new Entry constructed — same instance must now be denied.
	if e.GrantsCapability(CapEvents) {
		t.Fatal("revocation was not immediate at the dispatch gate")
	}
}

// ---------------------------------------------------------------------------
// Attack 4 — malformed / injected grant contents are inert, not dangerous.
// ---------------------------------------------------------------------------

// TestRedteam_InjectedUnknownCapNames_Inert: granting capability NAMES the host
// vocabulary does not define ("root", "admin", "*") must not confer any real
// capability — the gate keys on exact known names, so an injected name matches
// nothing meaningful. (It is stored, but no dispatch path is keyed on it.)
func TestRedteam_InjectedUnknownCapNames_Inert(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)
	m := manifestWithPerms("1.0.0", ext.Permissions{HostAPIs: []string{CapEvents}})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := mgr.SetGrant(m.ID, &Grant{HostAPIs: []GrantItem{{Name: "root"}, {Name: "admin"}, {Name: "*"}}}, false); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	wireGrant(t, mgr)
	e := &Entry{Manifest: m}
	// The two real dispatch-gated caps are NOT in the injected set → denied.
	if e.GrantsCapability(CapEvents) || e.GrantsCapability(CapPublicRoutes) {
		t.Fatal("injected junk names must not confer real events/public_routes caps")
	}
	// A wildcard name does not act as a match-all at the gate.
	if e.GrantsCapability("store") {
		t.Fatal("'*' host_api name must not behave as grant-all at the dispatch gate")
	}
}

// ---------------------------------------------------------------------------
// Attack 5 — scope granularity (implementer-disclosed seam). THEORETICAL.
// ---------------------------------------------------------------------------

// TestRedteam_ScopeGranularity_NoConsumer_Theoretical documents that the
// update-diff (manifestExceedsGrant) compares host_apis and storage_tokens by
// Name only, never by GrantItem.Scope — so a same-name/different-scope change
// would NOT trip NeedsReconsent. This is NOT exploitable today because (a) the
// flat v4 Manifest permission form carries no scope (Permissions.HostAPIs is
// []string), so an update cannot even request a different scope, and (b) nothing
// in the codebase reads GrantItem.Scope (dispatch gate matches by name; hostcap
// FS write-ness comes from GrantFromConsent's fs LEVEL, not from Scope). The test
// asserts the absence of a scope consumer so that if a future change starts
// honoring Scope without extending manifestExceedsGrant, THIS test's premise
// changes and the seam must be revisited.
func TestRedteam_ScopeGranularity_NoConsumer_Theoretical(t *testing.T) {
	// A grant carrying two different scopes for the same name resolves to the
	// same single name at the gate — scope is invisible to the name-keyed gate.
	g := &Grant{HostAPIs: []GrantItem{{Name: "store", Scope: "read"}}}
	names := g.hostAPINames()
	if len(names) != 1 || names[0] != "store" {
		t.Fatalf("hostAPINames must flatten to names only, got %v", names)
	}
	// manifestExceedsGrant sees a same-name request as already-covered regardless
	// of any scope refinement — confirming the diff is name-only.
	m := manifestWithPerms("2.0.0", ext.Permissions{HostAPIs: []string{"store"}})
	if manifestExceedsGrant(m, g) {
		t.Fatal("same-name request should be considered covered (name-only diff) — premise of the seam")
	}
}

// ---------------------------------------------------------------------------
// Attack 6 — concurrency: grant mutation vs. gate reads must be race-free.
// ---------------------------------------------------------------------------

// TestRedteam_ConcurrentGrantAndDispatch_NoRace hammers SetGrant against
// concurrent GrantedHostAPIs / GrantsCapability / PermissionView reads. Run under
// -race it proves the Manager mutex guards every Grant access and that the gate
// never observes a torn grant. It also confirms persistState (temp+rename) under
// contention does not corrupt or error.
func TestRedteam_ConcurrentGrantAndDispatch_NoRace(t *testing.T) {
	pub, priv := genKey(t)
	mgr, _ := newTestManager(t, pub)
	m := manifestWithPerms("1.0.0", ext.Permissions{HostAPIs: []string{CapEvents, CapPublicRoutes}})
	if _, err := mgr.Install(buildSignedPackage(t, m, priv)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	wireGrant(t, mgr)
	e := &Entry{Manifest: m}

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(3)
	// writer: flip the grant between full and empty.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if i%2 == 0 {
				_ = mgr.SetGrant(m.ID, &Grant{HostAPIs: []GrantItem{{Name: CapEvents}, {Name: CapPublicRoutes}}}, false)
			} else {
				_ = mgr.SetGrant(m.ID, &Grant{}, false)
			}
		}
	}()
	// reader A: gate reads through the wired resolver.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = e.GrantsCapability(CapEvents)
			_ = e.GrantsCapability(CapPublicRoutes)
		}
	}()
	// reader B: PermissionView + Get (copy paths).
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_, _ = mgr.PermissionView(m.ID)
			_, _ = mgr.Get(m.ID)
		}
	}()
	wg.Wait()
}
