package extension

import (
	"crypto/ed25519"
	"path/filepath"
	"strings"
	"testing"
)

// newPolicyGatedManager builds a Manager over the official root plus an
// injected source-list provider and a fixed policy, entirely through the
// constructor arguments of NewManagerWithTrust — never through the
// process-wide SetSourcesProvider/SetPolicyProvider, so these tests cannot
// leak state into (or race with) any other test in the package.
func newPolicyGatedManager(t *testing.T, officialPub ed25519.PublicKey, sourcesFn func() []PluginSource, policy Policy) (*Manager, *Registry) {
	t.Helper()
	reg := NewRegistry()
	tv := NewTieredVerifier(officialPub, sourcesFn)
	mgr, err := NewManagerWithTrust(filepath.Join(t.TempDir(), "plugins"), reg, tv, func() Policy { return policy })
	if err != nil {
		t.Fatal(err)
	}
	return mgr, reg
}

func TestPolicyGateVerifiedAllowsSourceSignedRejectsUnsigned(t *testing.T) {
	officialPub, _ := genKey(t)
	src, srcPriv := testSource(t, "myrepo")
	mgr, reg := newPolicyGatedManager(t, officialPub, func() []PluginSource { return []PluginSource{src} }, PolicyVerified)

	// 🟡 source-signed package: install + enable succeed under the "verified" policy.
	m := openManifest()
	m.ID = "com.example.verified"
	pkg := buildSignedPackage(t, m, srcPriv)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("install source-signed package under verified policy: %v", err)
	}
	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("enable source-signed package under verified policy: %v", err)
	}
	entry, ok := reg.Get(m.ID)
	if !ok || entry.ExecMode() != ExecGRPC {
		t.Fatalf("expected gRPC-registered entry, ok=%v entry=%+v", ok, entry)
	}
	if st, _ := mgr.Get(m.ID); st.Tier != TierVerified.String() {
		t.Fatalf("state tier=%q want verified", st.Tier)
	}

	// 🔴 unsigned package: install is rejected by the "verified" policy floor.
	m2 := openManifest()
	m2.ID = "com.example.unsigned"
	dir := writePackageDir(t, m2) // never signed
	out := filepath.Join(t.TempDir(), "unsigned.tar.gz")
	tarGzDir(t, dir, out)
	_, err := mgr.Install(out)
	if err == nil {
		t.Fatal("expected install to reject unsigned package under verified policy")
	}
	if !strings.Contains(err.Error(), "plugin_policy") {
		t.Fatalf("expected error to mention plugin_policy, got: %v", err)
	}
}

func TestPolicyGateAnyAllowsUnsignedGRPC(t *testing.T) {
	officialPub, _ := genKey(t)
	mgr, reg := newPolicyGatedManager(t, officialPub, nil, PolicyAny)

	// 🔴 unsigned package: "any" policy admits install AND enable, running gRPC.
	m := openManifest()
	m.ID = "com.example.anypolicy"
	dir := writePackageDir(t, m) // never signed
	out := filepath.Join(t.TempDir(), "unsigned.tar.gz")
	tarGzDir(t, dir, out)

	if _, err := mgr.Install(out); err != nil {
		t.Fatalf("install unsigned package under any policy: %v", err)
	}
	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("enable unsigned package under any policy: %v", err)
	}
	entry, ok := reg.Get(m.ID)
	if !ok || entry.ExecMode() != ExecGRPC {
		t.Fatalf("expected gRPC-registered entry, ok=%v entry=%+v", ok, entry)
	}
	st, _ := mgr.Get(m.ID)
	if st.ExecModeName != ExecGRPC.String() {
		t.Fatalf("state exec_mode=%q want grpc", st.ExecModeName)
	}
	if st.Tier != TierThirdParty.String() {
		t.Fatalf("state tier=%q want third-party", st.Tier)
	}
}

// TestPolicyGateSourceRemovalRegatesOnReEnable covers the delete-source
// re-judgment path: a 🟡 package installed and enabled while its vouching
// source is present must be refused on re-enable once that source is gone —
// re-gating on load (registerActive -> gate -> DetermineTier) must not trust a
// stale tier recorded in state.json.
func TestPolicyGateSourceRemovalRegatesOnReEnable(t *testing.T) {
	officialPub, _ := genKey(t)
	src, srcPriv := testSource(t, "myrepo")
	sources := []PluginSource{src} // mutated below to simulate the admin removing the source
	mgr, reg := newPolicyGatedManager(t, officialPub, func() []PluginSource { return sources }, PolicyVerified)

	m := openManifest()
	m.ID = "com.example.sourceremoval"
	pkg := buildSignedPackage(t, m, srcPriv)
	if _, err := mgr.Install(pkg); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("enable while source present: %v", err)
	}
	if _, ok := reg.Get(m.ID); !ok {
		t.Fatal("expected plugin registered while source present")
	}

	// Admin removes the source: the provider now yields zero sources, so this
	// package's signature no longer verifies against any trusted root.
	sources = nil

	if err := mgr.Disable(m.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok := reg.Get(m.ID); ok {
		t.Fatal("plugin still registered after disable")
	}

	if err := mgr.Enable(m.ID); err == nil {
		t.Fatal("expected re-enable to be rejected after the vouching source was removed")
	}
	if _, ok := reg.Get(m.ID); ok {
		t.Fatal("plugin must not be registered after a rejected re-enable")
	}
}
